import json
from pathlib import Path
import tempfile
import unittest

from analyze_live_contention import (analyze, analyze_run, cross_run_model_timing,
                                     environment_intervals, local_time_accounting,
                                     manual_wait_intervals, peak_concurrency, render,
                                     run_events_path, stamp, subtract_intervals, timeline, usage_totals)


class TimingAnalysisTests(unittest.TestCase):
    def test_run_journals_preserve_legacy_archives_and_prefer_new_brand(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            current = root / "workspace/.pwnmesh/runs/shared/events.jsonl"
            legacy = root / "workspace/.xloom/runs/shared/events.jsonl"
            self.assertEqual(run_events_path(root, "shared"), current)
            legacy.parent.mkdir(parents=True)
            self.assertEqual(run_events_path(root, "shared"), legacy)
            legacy.write_text("", encoding="utf-8")
            current.parent.mkdir(parents=True)
            self.assertEqual(run_events_path(root, "shared"), legacy)
            current.write_text("", encoding="utf-8")
            self.assertEqual(run_events_path(root, "shared"), current)
            for name, run_id, role in ((".pwnmesh", "shared", "reason"),
                                       (".xloom", "shared", "explore"),
                                       (".xloom", "legacy-only", "curate")):
                path = root / "workspace" / name / "runs" / run_id
                path.mkdir(parents=True, exist_ok=True)
                (path / "job.json").write_text(json.dumps({"run_id": run_id, "kind": role}), encoding="utf-8")
            (root / "manifest.json").write_text(json.dumps({
                "started": "2026-10-01T00:00:00Z", "completed_observed": "2026-10-01T00:00:10Z"}), encoding="utf-8")
            report = analyze(root)
            self.assertEqual({run["run_id"]: run["kind"] for run in report["runs"]},
                             {"shared": "reason", "legacy-only": "curate"})

    def test_docker_startup_supports_both_retained_brand_formats(self):
        for label in ("pwnmesh", "xloom"):
            for executable in ("pwnmesh", "xloom"):
                with self.subTest(label=label, executable=executable), tempfile.TemporaryDirectory() as directory:
                    root = Path(directory)
                    rows = []
                    for action, second in (("create", 0), ("start", 1),
                                           (f"exec_create: /usr/local/bin/{executable} worker --job private/path", 2),
                                           ("exec_start", 3)):
                        attrs = {label + ".namespace": "retained"}
                        if action.startswith("exec_"):
                            attrs["execID"] = "worker"
                        rows.append({"Actor": {"ID": "project", "Attributes": attrs},
                                     "Action": action, "timeNano": second * 10**9})
                    (root / "docker-events.jsonl").write_text("\n".join(map(json.dumps, rows)), encoding="utf-8")
                    spans, coverage = environment_intervals(root, {"namespace": "retained"}, [{"runner_attempts": [{}]}])
                    self.assertEqual(spans, [(0, 1), (2, 3)])
                    self.assertEqual(coverage["status"], "collected_startup_spans")

    def test_subtraction_unions_overlap_and_preserves_gaps(self):
        self.assertEqual(subtract_intervals([], [(0, 10)]), [])
        self.assertEqual(subtract_intervals([(0, 10)], []), [(0, 10)])
        self.assertEqual(subtract_intervals([(0, 0)], []), [])
        self.assertEqual(subtract_intervals([(0, 10), (5, 20)], [(2, 7), (4, 8), (12, 15), (19, 30)]),
                         [(0, 2), (8, 12), (15, 19)])
        self.assertEqual(subtract_intervals([(0, 10)], [(None, 10), (4, None), (10, 20)]), [(0, 10)])

    def test_docker_excludes_only_matching_startup_not_worker_lifetime(self):
        def event(action, second, namespace="test", exec_id=None):
            attrs = {"pwnmesh.namespace": namespace}
            if exec_id:
                attrs["execID"] = exec_id
            return {"Actor": {"ID": "project", "Attributes": attrs}, "Action": action, "timeNano": second * 10**9}
        rows = [event("create", 0), event("start", 2), event("die", 30), event("destroy", 40),
                event("exec_create: /usr/local/bin/pwnmesh worker --job private/path", 2, exec_id="worker"),
                event("exec_start", 3, exec_id="worker"), event("exec_die", 29, exec_id="worker"),
                event("exec_create: /bin/mv private/input private/output", 4, exec_id="bridge"),
                event("exec_start", 5, exec_id="bridge"), event("create", -10, "other")]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            (path / "docker-events.jsonl").write_text("\n".join(map(json.dumps, rows + rows)), encoding="utf-8")
            spans, coverage = environment_intervals(path, {"namespace": "test"}, [{"runner_attempts": [{}]}])
            self.assertEqual(spans, [(0, 2), (2, 3)])
            self.assertEqual(coverage["status"], "collected_startup_spans")
            self.assertNotIn("private", json.dumps(coverage))
            _, partial = environment_intervals(path, {"namespace": "test"}, [{"runner_attempts": [{}, {}]}])
            self.assertEqual(partial["status"], "partial")
            missing, unknown = environment_intervals(path, {}, [])
            self.assertEqual(missing, [])
            self.assertEqual(unknown["status"], "not_collected")

    def test_manual_wait_requires_failed_run_and_authorization_preserves_other_work(self):
        at = lambda second: f"2026-09-28T00:00:{second:02d}Z"
        runs = [{"run_id": "failed", "status": "failed", "finished": at(10),
                 "runner_attempts": [{"started": at(1), "finished": at(10)}]},
                {"run_id": "other", "status": "success", "finished": at(20),
                 "runner_attempts": [{"started": at(12), "finished": at(20)}]},
                {"run_id": "next", "status": "success", "finished": at(40),
                 "runner_attempts": [{"started": at(31), "finished": at(40)}]}]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            record = {"previous_run_id": "failed", "at": at(30), "http_status": 200, "reason": "private"}
            (path / "manual-recovery.json").write_text(json.dumps(record), encoding="utf-8")
            spans, coverage = manual_wait_intervals(path, runs)
            self.assertEqual(spans, [(stamp(at(10)), stamp(at(12))), (stamp(at(20)), stamp(at(30)))])
            self.assertEqual(coverage["resolved"], 1)
            self.assertNotIn("private", json.dumps(coverage))
            runs[-1]["runner_attempts"][0]["finished"] = None
            runs[-1]["runner_attempts"][0]["started"] = at(20)
            self.assertEqual(manual_wait_intervals(path, runs)[0], [])
            runs[-1]["runner_attempts"][0]["finished"] = at(40)
            record["previous_run_id"] = "other"
            (path / "manual-recovery.json").write_text(json.dumps(record), encoding="utf-8")
            self.assertEqual(manual_wait_intervals(path, runs)[0], [])

    def test_local_residual_keeps_http_backoff_and_subtracts_parallel_union(self):
        at = lambda second: f"2026-09-28T00:00:{second:02d}Z"
        start = stamp(at(0))
        runs = [{"run_id": name, "requests": [{"index": 1}], "warnings": [],
                 "runner_attempts": [{"started": at(0), "finished": at(12)}]}
                for name in ("a", "b")]
        http = [{"run_id": name, "logical_request_index": 1, "started_at": at(left), "finished_at": at(right)}
                for name, left, right in (("a", 1, 3), ("a", 5, 8), ("b", 6, 10))]
        report = {"runs": runs, "http_attempts": http, "http_observation_status": "collected",
                  "coverage": {"all_run_evidence_present": True, "incomplete_model_observations": 0}}
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            result = local_time_accounting(path, {}, report,
                                           [(start + 1, start + 8), (start + 6, start + 10)],
                                           [(start + 7, start + 11)], (start, start + 12))
            self.assertEqual(result["model_call_union_seconds"], 9)
            self.assertEqual(result["provider_http_union_seconds"], 7)
            self.assertEqual(result["after_model_environment_manual_seconds"], 3)
            self.assertEqual(result["after_provider_environment_manual_seconds"], 5)
            self.assertEqual(result["remaining_tool_seconds"], 1)
            self.assertEqual(result["remaining_unattributed_seconds"], 2)
            self.assertIsNone(result["observed_environment_union_seconds"])
            report["http_attempts"] = http[:2]
            report["coverage"]["incomplete_model_observations"] = 1
            result = local_time_accounting(path, {}, report, [], [], (start, start + 12))
            self.assertIsNone(result["after_provider_environment_manual_seconds"])
            self.assertEqual(result["model_call_coverage"], "partial")

    def test_provider_coverage_rejects_unclosed_or_unmatched_attempts_even_after_a_closed_retry(self):
        at = lambda second: f"2026-09-28T00:00:{second:02d}Z"
        start = stamp(at(0))
        closed = {"run_id": "a", "logical_request_index": 1, "started_at": at(1), "finished_at": at(2)}
        report = {"runs": [{"run_id": "a", "requests": [{"index": 1}], "warnings": [], "runner_attempts": []}],
                  "http_observation_status": "collected",
                  "coverage": {"all_run_evidence_present": True, "incomplete_model_observations": 0}}
        invalid = [dict(closed, finished_at="0001-01-01T00:00:00Z"),
                   dict(closed, finished_at=None), dict(closed, started_at=None),
                   dict(closed, started_at=at(3)), dict(closed, logical_request_index=None)]
        with tempfile.TemporaryDirectory() as directory:
            for attempt in invalid:
                with self.subTest(attempt=attempt):
                    # A previous closed attempt for the same logical call cannot
                    # hide a later retry with missing timing or matching data.
                    report["http_attempts"] = [closed, attempt]
                    result = local_time_accounting(Path(directory), {}, report, [], [], (start, start + 10))
                    self.assertEqual(result["provider_http_coverage"], "incomplete_or_uncollected")
                    self.assertIsNone(result["provider_http_union_seconds"])
                    self.assertIsNone(result["after_provider_environment_manual_seconds"])

    def test_environment_and_model_overlap_is_removed_once(self):
        at = lambda second: f"2026-09-28T00:00:{second:02d}Z"
        start = stamp(at(0))
        report = {"runs": [{"runner_attempts": [], "requests": [], "warnings": []}],
                  "http_attempts": [], "http_observation_status": "not_collected",
                  "coverage": {"all_run_evidence_present": True, "incomplete_model_observations": 0}}
        events = [{"Actor": {"ID": "project", "Attributes": {"pwnmesh.namespace": "test"}},
                   "Action": action, "timeNano": int((start + second) * 10**9)}
                  for action, second in (("create", -2), ("start", 3))]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            (path / "docker-events.jsonl").write_text("\n".join(map(json.dumps, events)), encoding="utf-8")
            result = local_time_accounting(path, {"namespace": "test"}, report,
                                           [(start + 2, start + 8)], [], (start, start + 10))
        self.assertEqual(result["observed_environment_union_seconds"], 3)
        self.assertEqual(result["model_call_union_seconds"], 6)
        self.assertEqual(result["excluded_union_seconds"], 8)
        self.assertEqual(result["after_model_environment_manual_seconds"], 2)

    def test_parallel_models_count_distinct_successful_foreground_runs(self):
        def call(start, end, **kwargs):
            return {"started": f"2026-09-28T00:00:{start:02d}Z", "finished": f"2026-09-28T00:00:{end:02d}Z",
                    "stream": "main", "kind": "turn", **kwargs}
        def run(calls, **kwargs):
            return {"requests": calls, "missing_evidence_files": [], **kwargs}
        from analyze_live_contention import stamp
        window = (stamp("2026-09-28T00:00:00Z"), stamp("2026-09-28T00:00:20Z"))
        one = run([call(1, 10), call(3, 12), call(1, 20, stream="replan"), call(1, 20, kind="summary")])
        result = cross_run_model_timing([one], window)
        self.assertEqual(result["peak_concurrent_runs"], 1)
        self.assertEqual(result["two_or_more_active_wall_seconds"], 0)
        result = cross_run_model_timing([one, run([call(5, 15), call(0, 20, failed=True)])], window)
        self.assertEqual(result["peak_concurrent_runs"], 2)
        self.assertEqual(result["two_or_more_active_wall_seconds"], 7)
        self.assertEqual(result["summed_per_run_active_seconds"], 21)
        self.assertEqual(result["active_wall_seconds"], 14)
        self.assertEqual(result["status"], "collected")
        partial = cross_run_model_timing([one, run([], missing_evidence_files=["events.jsonl"])], window)
        self.assertEqual(partial["status"], "partial")
        self.assertEqual(partial["peak_concurrent_runs"], 1)
        unknown = cross_run_model_timing([run([], missing_evidence_files=["events.jsonl"])], window)
        self.assertEqual(unknown["status"], "unknown")
        self.assertIsNone(unknown["peak_concurrent_runs"])
        self.assertEqual(cross_run_model_timing([run([])], window)["peak_concurrent_runs"], 0)
        self.assertEqual(cross_run_model_timing([one, run([call(12, 20)])], window)["two_or_more_active_wall_seconds"], 0)

    def test_request_action_link_omits_transcript_and_preserves_role_effort(self):
        events = [
            {"type": "model_call_start", "at": "2026-09-28T00:00:00Z", "request": {"kind": "turn"}},
            {"type": "model_call_end", "at": "2026-09-28T00:00:01Z", "request": {"kind": "turn"}},
            {"type": "message_end", "message": {"role": "assistant", "content": [
                {"type": "thinking", "thinking": "private-model-thoughts"},
                {"type": "tool_use", "id": "read", "name": "read_graph", "input": {"section": "facts", "private": "private-args"}}]}},
            {"type": "tool_start", "tool_id": "read", "tool_name": "read_graph", "at": "2026-09-28T00:00:02Z"},
            {"type": "tool_end", "tool_id": "read", "tool_name": "read_graph", "at": "2026-09-28T00:00:03Z"},
            {"type": "model_call_start", "at": "2026-09-28T00:00:04Z", "request": {"kind": "turn"}},
            {"type": "model_call_end", "at": "2026-09-28T00:00:05Z", "request": {"kind": "turn"}},
        ]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "job.json").write_text(json.dumps({"budget": {"reasoning_effort": "high"}}), encoding="utf-8")
            (root / "events.jsonl").write_text("\n".join(map(json.dumps, events)), encoding="utf-8")
            result, _, _ = analyze_run(root, {})
        self.assertEqual(result["job_reasoning_effort"], "high")
        self.assertEqual(result["requests"][0]["following_tools"], [{"name": "read_graph", "section": "facts"}])
        self.assertEqual(result["requests"][1]["following_tools"], [])
        self.assertNotIn("private", json.dumps(result))

    def test_parallel_intervals_count_once_and_clip(self):
        result = timeline([(0, 5), (2, 7), (9, 14)], [(4, 8), (5, 6)], (1, 11))
        self.assertEqual(result["wall_seconds"], 10)
        self.assertEqual(result["model_active_seconds"], 8)
        self.assertEqual(result["tool_active_seconds"], 4)
        self.assertEqual(result["overlap_seconds"], 3)
        self.assertEqual(result["model_only_seconds"], 5)
        self.assertEqual(result["tool_only_seconds"], 1)
        self.assertEqual(result["neither_seconds"], 1)
        self.assertEqual(peak_concurrency([(0, 2), (1, 3), (3, 5)]), 2)

    def test_request_usage_counted_once_despite_transcript_copy(self):
        at = lambda second: f"2026-09-23T00:00:{second:02d}Z"
        usage = {"input_tokens": 10, "output_tokens": 4}
        events = [
            {"type": "model_call_start", "at": at(1), "request": {"kind": "turn"}},
            {"type": "model_call_end", "at": at(4), "request": {"kind": "turn", "duration_ms": 3000, "usage": usage}},
            {"type": "message_end", "message": {"role": "assistant", "usage": usage, "content": [{"type": "tool_use", "id": "read-1", "name": "read_graph", "input": {"section": "facts", "private": "do not retain"}}]}},
            {"type": "tool_start", "at": at(4), "tool_id": "read-1", "tool_name": "read_graph"},
            {"type": "decision_operation", "text": json.dumps({"op": "read_graph", "state_changed": True, "failed": True})},
            {"type": "tool_end", "at": at(5), "tool_id": "read-1", "tool_name": "read_graph", "error": "state_changed: hidden server detail"},
            {"type": "model_call_start", "at": at(6), "request": {"kind": "summary"}},
            {"type": "model_call_end", "at": at(8), "request": {"kind": "summary", "duration_ms": 2000, "failed": True, "usage": usage}},
            {"type": "context_compaction_prepared", "compaction": {"usage": usage}},
        ]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            (path / "events.jsonl").write_text("\n".join(json.dumps(event) for event in events), encoding="utf-8")
            result, model, tools = analyze_run(path, {"run_id": "r1", "kind": "reason", "started": at(0), "finished": at(9)})
        self.assertEqual(result["model_calls"], 2)
        self.assertEqual(result["summary_calls"], 1)
        self.assertEqual(result["failed_model_calls"], 1)
        self.assertEqual(result["reported_usage"]["input_tokens"], 20)
        self.assertEqual(result["reported_usage"]["output_tokens"], 8)
        self.assertEqual(result["usage_calls"], 2)
        self.assertEqual(result["tools"][0]["section"], "facts")
        self.assertEqual(result["tools"][0]["model_requests_after_conflict"], [2])
        self.assertEqual(result["tools"][0]["duration_ms"], 1000)
        self.assertEqual(len(model), 2)
        self.assertEqual(len(tools), 1)
        self.assertNotIn("private", json.dumps(result))
        self.assertNotIn("hidden server detail", json.dumps(result))
        self.assertEqual(usage_totals([{"usage": usage}, {}])["usage_status"], "partial")

    def test_complete_fixture_has_external_updates_and_http_retry(self):
        at = lambda second: f"2026-09-23T00:00:{second:02d}Z"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            def save(name, value):
                (root / name).write_text(json.dumps(value), encoding="utf-8")
            save("manifest.json", {"started": at(0), "completed_observed": at(21), "model": "fixture", "source_commit": "test", "reasoning_effort": "max"})
            runs = [{"run_id": "decide", "kind": "reason", "started": at(1), "finished": at(20), "status": "success"},
                    {"run_id": "execute-a", "kind": "explore", "started": at(4), "finished": at(12), "status": "success"},
                    {"run_id": "execute-b", "kind": "explore", "started": at(8), "finished": at(16), "status": "success"}]
            save("runs.json", runs)
            save("state-events.json", [{"op": "fact", "run_id": "live@execute-a", "revision": 3, "created_at": at(10)},
                                       {"op": "complete", "run_id": "live@decide", "revision": 4, "created_at": at(19)}])
            save("validation.json", {"passed": True, "failures": []})
            save("http-observations.json", [{"request_id": 1, "run_id": "decide", "started_at": at(2), "finished_at": at(3), "http_status": 503, "usage": {"input_tokens": 1000}},
                                            {"request_id": 2, "run_id": "decide", "started_at": at(4), "finished_at": at(6), "http_status": 200,
                                             "errors": ["upstream_body_read_failed", "request_cancelled"],
                                             "usage": {"input_tokens": 7, "output_tokens": 3, "cache_read_input_tokens": 0}}])
            for run in runs:
                path = root / "workspace" / ".pwnmesh" / "runs" / run["run_id"]
                path.mkdir(parents=True)
                (path / "job.json").write_text(json.dumps({"run_id": run["run_id"], "kind": run["kind"]}), encoding="utf-8")
                (path / "session.json").write_text("{}", encoding="utf-8")
                events = []
                if run["kind"] == "reason":
                    events = [{"type": "model_call_start", "at": at(1), "request": {"kind": "turn"}},
                              {"type": "model_call_end", "at": at(7), "request": {"kind": "turn", "duration_ms": 6000, "usage": {"input_tokens": 7, "output_tokens": 3, "cache_read_input_tokens": 100}}},
                              {"type": "message_end", "message": {"role": "assistant", "content": [{"type": "tool_use", "id": "commit-1", "input": {"op": "commit"}}]}},
                              {"type": "tool_start", "at": "2026-09-23T00:00:19.100000Z", "tool_id": "commit-1", "tool_name": "graph_action"},
                              {"type": "decision_operation", "text": json.dumps({"op": "decision_commit", "committed": True})},
                              {"type": "tool_end", "at": "2026-09-23T00:00:19.400000Z", "tool_id": "commit-1", "tool_name": "graph_action"}]
                (path / "events.jsonl").write_text("\n".join(json.dumps(event) for event in events), encoding="utf-8")
            report = analyze(root)
            manifest = json.loads((root / "manifest.json").read_text(encoding="utf-8"))
            manifest["http_observation_mode"] = "proxy"
            save("manifest.json", manifest)
            self.assertEqual(analyze(root), report)
            save("validation-reviewed.json", {"passed": True, "failures": [], "review_reason": "independent evidence review"})
            reviewed_report = analyze(root)
            path = root / "workspace" / ".pwnmesh" / "runs" / "decide" / "events.jsonl"
            events = [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()]
            next(event for event in events if event["type"] == "model_call_end")["request"]["failed"] = True
            path.write_text("\n".join(json.dumps(event) for event in events), encoding="utf-8")
            failed_report = analyze(root)
        self.assertAlmostEqual(report["project_wall_seconds"], 19.4, places=5)
        self.assertTrue(render(report).startswith("# PwnMesh 真实模型并发验收与耗时\n"))
        self.assertAlmostEqual(report["local_time_accounting"]["after_model_environment_manual_seconds"], 13.4, places=5)
        self.assertEqual(report["local_time_accounting"]["environment"]["status"], "not_collected")
        self.assertIn("残余包含必要执行和未观测环境", render(report))
        self.assertEqual(report["authoritative_event_wall_seconds_lower_bound"], 19)
        self.assertEqual(report["end_basis"], "successful_complete_commit_receipt_observed")
        self.assertEqual(report["reported_usage"]["input_tokens"], 7)
        self.assertEqual(report["http_retries_in_matched_logical_calls"], 1)
        self.assertEqual(report["proxy_reported_usage"]["input_tokens"], 1007)
        self.assertEqual(report["usage_difference_calls"], 1)
        self.assertEqual(report["usage_comparisons"][0]["app_minus_proxy"]["cache_read_input_tokens"], 100)
        self.assertEqual(report["proxy_error_class_counts"]["http_attempt_failed"], 1)
        self.assertEqual(report["proxy_error_class_counts"]["stream_close_after_app_success"], 1)
        self.assertEqual(report["http_attempts"][1]["errors"], ["upstream_body_read_failed", "request_cancelled"])
        self.assertEqual(failed_report["proxy_error_class_counts"]["proxy_error_with_failed_logical_call"], 1)
        self.assertEqual(report["coverage"]["peak_concurrent_execute_runs"], 2)
        self.assertTrue(report["coverage"]["decision_saw_external_fact_updates"])
        self.assertEqual(len(report["external_business_updates_during_decisions"]), 1)
        self.assertTrue(report["coverage"]["all_run_evidence_present"])
        self.assertIn("本次工具响应未观测到 state_changed", render(report))
        self.assertIn("此统计不包含调度器主动取消过期决策", render(report))
        self.assertTrue(reviewed_report["validation_review_applied"])
        self.assertIn("原始 validation.json 未被覆盖", render(reviewed_report))
        self.assertIn("independent evidence review", render(reviewed_report))
        self.assertNotIn("marker suffix", render(reviewed_report))
        self.assertNotIn("arithmetic-correction", render(reviewed_report))

    def test_direct_mode_without_http_observations_keeps_application_usage(self):
        at = lambda second: f"2026-09-23T00:00:{second:02d}Z"
        for file_present in (False, True):
            with self.subTest(empty_file=file_present), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                (root / "manifest.json").write_text(json.dumps({
                    "started": at(0), "completed_observed": at(5), "http_observation_mode": "direct",
                }), encoding="utf-8")
                (root / "runs.json").write_text(json.dumps([
                    {"run_id": "direct-run", "kind": "reason", "started": at(1), "finished": at(4)},
                ]), encoding="utf-8")
                if file_present:
                    (root / "http-observations.json").write_text("[]", encoding="utf-8")
                path = root / "workspace" / ".pwnmesh" / "runs" / "direct-run"
                path.mkdir(parents=True)
                events = [
                    {"type": "model_call_start", "at": at(1), "request": {"kind": "turn"}},
                    {"type": "model_call_end", "at": at(3), "request": {
                        "kind": "turn", "duration_ms": 2000,
                        "usage": {"input_tokens": 17, "output_tokens": 5},
                    }},
                ]
                (path / "events.jsonl").write_text("\n".join(json.dumps(event) for event in events), encoding="utf-8")
                report = analyze(root)
                self.assertEqual(report["http_observation_mode"], "direct")
                self.assertEqual(report["http_observation_status"], "not_collected")
                for key in ("http_attempt_count", "http_retries_in_matched_logical_calls", "unmatched_http_attempts",
                            "http_timing_ms", "http_stage_cumulative_seconds", "proxy_reported_usage",
                            "proxy_error_class_counts", "thinking_chars", "output_chars", "usage_difference_calls"):
                    self.assertIsNone(report[key], key)
                self.assertEqual(report["model_calls"], 1)
                self.assertEqual(report["usage_calls"], 1)
                self.assertEqual(report["usage_status"], "reported_only")
                self.assertEqual(report["reported_usage"]["input_tokens"], 17)
                self.assertEqual(report["reported_usage"]["output_tokens"], 5)
                self.assertEqual(report["timing"]["model_active_seconds"], 2)
                self.assertEqual(report["usage_comparisons"][0]["status"], "not_comparable")
                self.assertIsNone(report["usage_comparisons"][0]["app_minus_proxy"])
                rendered = render(report)
                self.assertIn("HTTP 观测未采集（direct 直连）", rendered)
                self.assertIn("| 代理各 HTTP 尝试（未采集） | 未知 | 未知 | 未知 | 未知 |", rendered)
                self.assertIn("| 应用逻辑请求 | 17 | 5 |", rendered)
                self.assertNotIn("HTTP 尝试 0 次", rendered)
                self.assertNotIn("到首事件等待 0.000 秒", rendered)
                self.assertNotIn("0 次存在差异", rendered)

    def test_ambiguous_same_second_commits_keep_coarse_event_boundary(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "manifest.json").write_text(json.dumps({
                "started": "2026-09-28T00:00:00Z", "completed_observed": "2026-09-28T00:00:12Z",
                "acceptance_passed": True, "time_to_acceptance_seconds": 10.5,
            }), encoding="utf-8")
            (root / "validation.json").write_text(json.dumps({"passed": True}), encoding="utf-8")
            (root / "runs.json").write_text(json.dumps([{"run_id": "reason", "kind": "reason"}]), encoding="utf-8")
            (root / "state-events.json").write_text(json.dumps([{
                "op": "complete", "run_id": "live@reason", "created_at": "2026-09-28T00:00:10Z",
            }]), encoding="utf-8")
            path = root / "workspace" / ".pwnmesh" / "runs" / "reason"
            path.mkdir(parents=True)
            events = []
            for key, begin, end in (("earlier-plan", 100, 200), ("actual-completion", 700, 900)):
                events += [
                    {"type": "message_end", "message": {"role": "assistant", "content": [
                        {"type": "tool_use", "id": key, "input": {"op": "commit"}}]}},
                    {"type": "tool_start", "tool_id": key, "tool_name": "graph_action", "at": f"2026-09-28T00:00:10.{begin:03d}Z"},
                    {"type": "decision_operation", "text": json.dumps({"op": "decision_commit", "committed": True})},
                    {"type": "tool_end", "tool_id": key, "tool_name": "graph_action", "at": f"2026-09-28T00:00:10.{end:03d}Z"},
                ]
            (path / "events.jsonl").write_text("\n".join(map(json.dumps, events)), encoding="utf-8")
            report = analyze(root)
        self.assertEqual(report["end_basis"], "authoritative_complete_state_event")
        self.assertIsNone(report["completion_commit_window"])
        self.assertEqual(report["authoritative_event_precision_seconds"], 1)
        self.assertEqual(report["project_wall_seconds"], 10)
        self.assertIsNone(report["time_to_acceptance_seconds"])
        self.assertIn("10.000–11.000 秒（整秒事件范围", render(report))

    def test_acceptance_time_requires_original_pass_and_valid_measurement(self):
        cases = [
            (True, True, 8.25, True), (False, True, 8.25, False),
            (True, False, 8.25, False), (True, True, None, False),
            (True, True, 4, False), (True, True, True, False),
            (True, True, "8.25", False), (True, True, float("inf"), False),
        ]
        for passed, recorded, elapsed, expected in cases:
            with self.subTest(case=(passed, recorded, elapsed)), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                (root / "manifest.json").write_text(json.dumps({
                    "started": "2026-09-28T00:00:00Z", "completed_observed": "2026-09-28T00:00:05Z",
                    "acceptance_passed": recorded, "time_to_acceptance_seconds": elapsed,
                    "collection_and_validation_seconds": 3.25, "cleanup_seconds": 0.1,
                }), encoding="utf-8")
                (root / "validation.json").write_text(json.dumps({"passed": passed}), encoding="utf-8")
                # A later manual review must never retroactively claim the
                # original failed delivery passed at the original timestamp.
                (root / "validation-reviewed.json").write_text(json.dumps({"passed": True}), encoding="utf-8")
                report = analyze(root)
            self.assertEqual(report["time_to_acceptance_seconds"], elapsed if expected else None)
            self.assertEqual(report["acceptance_timing_status"], "measured_original_pass" if expected else "not_verified")
            self.assertEqual(report["collection_and_validation_seconds"], 3.25)
            self.assertEqual(report["cleanup_seconds"], 0.1)
            self.assertIn("原始独立验收通过：" if expected else "原始独立验收通过耗时未验证", render(report))


if __name__ == "__main__":
    unittest.main()
