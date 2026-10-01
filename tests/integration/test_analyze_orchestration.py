import json
from pathlib import Path
import tempfile
import unittest

from analyze_live_contention import analyze, execution_role, graph_observations, render


def at(second):
    return f"2026-09-28T00:00:{second:02d}Z"


def save(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value), encoding="utf-8")


def fixture(root, attempts, finish=12):
    save(root / "manifest.json", {"started": at(0), "completed_observed": at(finish),
                                 "http_observation_mode": "direct"})
    save(root / "runs.json", attempts)
    save(root / "validation.json", {"passed": True})


def attempt(run, start, end, kind="explore"):
    return {"run_id": run, "kind": kind, "started": at(start), "finished": at(end), "status": "success"}


def journal(root, run, spans, kind="explore", review=False):
    directory = root / "workspace" / ".pwnmesh" / "runs" / run
    save(directory / "job.json", {"run_id": run, "kind": kind,
                                  "intent": {"id": "step-" + run},
                                  "input_view": {"steps": [{"id": "step-" + run, "dispute_id": "d1" if review else ""}]}})
    save(directory / "session.json", {})
    events = []
    for start, end in spans:
        events += [{"type": "model_call_start", "at": at(start), "request": {"kind": "turn"}},
                   {"type": "model_call_end", "at": at(end), "request": {"duration_ms": (end - start) * 1000}}]
    (directory / "events.jsonl").write_text("\n".join(json.dumps(event) for event in events), encoding="utf-8")
    return directory


def graph(directory, run, start, end, status="succeeded"):
    save(directory / "graph" / "graph.json", {
        "schema_version": 1, "version": "execute-v1", "run_id": run,
        "nodes": [{"id": "agent", "kind": "agent", "status": status, "started_at": at(start),
                   "finished_at": at(end) if end is not None else "0001-01-01T00:00:00Z",
                   "output": {"value": "private model body", "artifacts": [{"path": "private path"}]},
                   "error": "private failure", "reason": "private reason"}]})


def graph_tool_call(directory, call, key, start=2, end=8):
    events = [{"type": "message_end", "at": at(start), "message": {"role": "assistant", "content": [
        {"type": "tool_use", "id": call, "name": "run_graph", "input": {"key": key, "nodes": [{"command": "private command"}]}}]}},
        {"type": "tool_start", "at": at(start), "tool_id": call, "tool_name": "run_graph"},
        {"type": "tool_end", "at": at(end), "tool_id": call, "tool_name": "run_graph"}]
    with (directory / "events.jsonl").open("a", encoding="utf-8") as stream:
        stream.write("\n" + "\n".join(json.dumps(event) for event in events))


class OrchestrationTimingTests(unittest.TestCase):
    def test_three_worker_acceptance_reports_concurrent_union(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            spans = [("a", 0, 20), ("b", 5, 15), ("c", 10, 25)]
            fixture(root, [attempt(run, start, end) for run, start, end in spans], finish=30)
            for run, start, end in spans:
                journal(root, run, [(start, end)])
            save(root / "parallel-validation.json", {
                "passed": True, "model_overlap_seconds": 15,
                "workers": [{"run_id": run} for run, _, _ in spans], "failures": []})
            report = analyze(root)
        self.assertEqual(report["model_parallelism"]["peak_concurrent_runs"], 3)
        self.assertEqual(report["model_parallelism"]["two_or_more_active_wall_seconds"], 15)
        self.assertIn("至少两个来源 Worker 同时成功请求模型的墙钟并集 15.000 秒", render(report))
        self.assertNotIn("两个来源 Worker 成功模型请求交集", render(report))

    def test_parallel_roles_and_nodes_use_sums_and_clipped_unions(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root, [attempt("a", 1, 7), attempt("b", 3, 9), attempt("review", 8, 12),
                           attempt("curator", 8, 11, "curate")], finish=10)
            for run, start, end in (("a", 1, 6), ("b", 3, 8), ("review", 8, 12)):
                path = journal(root, run, [(start, end)], review=run == "review")
                graph(path, run, start, end)
            journal(root, "curator", [(8, 11)], kind="curate")
            report = analyze(root)
        execute = report["role_timing"]["explore"]
        self.assertEqual(execute["model_calls"], 2)
        self.assertEqual(execute["runner"]["cumulative_seconds"], 12)
        self.assertEqual(execute["runner"]["active_wall_seconds"], 8)
        self.assertEqual(execute["model"]["cumulative_seconds"], 10)
        self.assertEqual(execute["model"]["active_wall_seconds"], 7)
        self.assertEqual(report["role_timing"]["review"]["model"]["cumulative_seconds"], 4)
        self.assertEqual(report["role_timing"]["review"]["model"]["active_wall_seconds"], 2)
        self.assertEqual(report["role_timing"]["curate"]["model"]["active_wall_seconds"], 2)
        node = report["graph_timing"]["by_node"]["agent"]
        self.assertEqual(node["cumulative_seconds"], 14)
        self.assertEqual(node["active_wall_seconds"], 9)
        self.assertEqual(report["graph_timing"]["collected_runs"], 3)
        self.assertNotIn("private", json.dumps(report))
        self.assertIn("不能称为纯图框架开销", render(report))

    def test_resumed_runner_preserves_attempts_without_recounting_journal(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            # Deliberately reversed file order; attempt ordering uses timestamps.
            fixture(root, [attempt("resumed", 7, 10), attempt("gap-worker", 4, 6), attempt("resumed", 1, 3)])
            journal(root, "resumed", [(1, 2), (7, 9)])
            journal(root, "gap-worker", [(4, 5)])
            report = analyze(root)
        resumed = next(run for run in report["runs"] if run["run_id"] == "resumed")
        self.assertEqual(resumed["model_calls"], 2)
        self.assertEqual(resumed["wall_seconds"], 9)
        self.assertEqual(len(resumed["runner_attempts"]), 2)
        role = report["role_timing"]["explore"]
        self.assertEqual(role["runs"], 2)
        self.assertEqual(role["runner_attempts"], 3)
        self.assertEqual(role["runner"]["cumulative_seconds"], 7)
        self.assertEqual(role["runner"]["active_wall_seconds"], 7)
        self.assertEqual(role["model_calls"], 3)
        self.assertEqual(report["coverage"]["peak_concurrent_execute_runs"], 1)

    def test_missing_evidence_is_unknown_not_zero(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root, [attempt("missing", 1, 4)])
            report = analyze(root)
        role = report["role_timing"]["execute_unknown"]
        self.assertIsNone(role["model_calls"])
        self.assertIsNone(role["model"]["cumulative_seconds"])
        self.assertIsNone(role["tool"]["active_wall_seconds"])
        self.assertEqual(report["graph_timing"]["status"], "not_collected")
        self.assertIsNone(report["graph_timing"]["node_intervals"]["active_wall_seconds"])
        self.assertIn("图节点时间未知", render(report))

    def test_interrupted_node_does_not_borrow_project_finish(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root, [attempt("interrupted", 1, 7)])
            path = journal(root, "interrupted", [])
            graph(path, "interrupted", 2, None, "running")
            report = analyze(root)
        node = report["graph_timing"]["by_node"]["agent"]
        self.assertEqual(node["incomplete_intervals"], 1)
        self.assertEqual(node["status"], "unknown")
        self.assertIsNone(node["active_wall_seconds"])
        run = report["graph_timing"]["per_run"]["interrupted"]
        self.assertIsNone(run["peak_concurrent_nodes"])
        self.assertIsNone(run["uncovered_envelope_seconds"])

    def test_inner_parallelism_excludes_other_workers_and_preserves_phase_coverage(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root, [attempt("parallel", 0, 12), attempt("other", 1, 10)], finish=10)
            path = journal(root, "parallel", [])
            save(path / "graph" / "graph.json", {
                "schema_version": 1, "version": "parallel-v1", "run_id": "parallel",
                "nodes": [
                    {"id": "a", "kind": "function", "status": "succeeded", "ready_at": at(0),
                     "started_at": at(1), "finished_at": at(5), "run_duration_ms": 2500,
                     "verify_duration_ms": 10, "recovery_verify_duration_ms": 5},
                    {"id": "b", "kind": "function", "status": "succeeded", "ready_at": at(1),
                     "started_at": at(2), "finished_at": at(6), "run_duration_ms": 3500,
                     "verify_duration_ms": 0, "reconcile_duration_ms": 20},
                    {"id": "join", "kind": "agent", "status": "succeeded", "ready_at": at(6),
                     "started_at": at(8), "finished_at": at(12), "condition_duration_ms": 2},
                    {"id": "unused", "kind": "function", "status": "skipped", "condition_duration_ms": 1}]})
            graph(journal(root, "other", []), "other", 1, 10)
            report = analyze(root)
        run = report["graph_timing"]["per_run"]["parallel"]
        self.assertEqual(run["cumulative_seconds"], 12)
        self.assertEqual(run["active_wall_seconds"], 7)
        self.assertEqual(run["peak_concurrent_nodes"], 2)
        self.assertEqual(run["observed_overlap_node_seconds"], 3)
        self.assertEqual(run["observed_envelope_seconds"], 9)
        self.assertEqual(run["uncovered_envelope_seconds"], 2)
        self.assertEqual(run["ready_to_start"]["cumulative_seconds"], 4)
        self.assertEqual(run["by_kind"]["function"]["active_wall_seconds"], 5)
        self.assertEqual(run["by_kind"]["agent"]["active_wall_seconds"], 2)
        self.assertEqual(run["phase_duration_ms"]["run_duration_ms"]["count"], 2)
        self.assertEqual(run["phase_duration_ms"]["run_duration_ms"]["sum"], 6000)
        self.assertEqual(run["phase_duration_ms"]["verify_duration_ms"]["count"], 2)
        self.assertEqual(run["phase_duration_ms"]["condition_duration_ms"]["sum"], 3)
        self.assertEqual(run["phase_duration_ms"]["recovery_verify_duration_ms"]["sum"], 5)
        self.assertEqual(run["phase_duration_ms"]["reconcile_duration_ms"]["sum"], 20)
        other = report["graph_timing"]["per_run"]["other"]
        self.assertEqual(other["peak_concurrent_nodes"], 1)
        self.assertEqual(other["phase_duration_ms"]["run_duration_ms"], {"count": 0})
        self.assertIsNone(other["ready_to_start"]["cumulative_seconds"])

    def test_invalid_phase_values_do_not_become_zero_or_invalidate_valid_spans(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root, [attempt("invalid", 1, 4)])
            path = journal(root, "invalid", [])
            graph(path, "invalid", 1, 4)
            checkpoint = json.loads((path / "graph" / "graph.json").read_text())
            checkpoint["nodes"][0].update(run_duration_ms=-1, verify_duration_ms=True,
                                          reconcile_duration_ms="private value", condition_duration_ms=float("nan"),
                                          recovery_verify_duration_ms=float("inf"))
            save(path / "graph" / "graph.json", checkpoint)
            report = analyze(root)
        run = report["graph_timing"]["per_run"]["invalid"]
        self.assertEqual(run["active_wall_seconds"], 3)
        self.assertTrue(all(value == {"count": 0} for value in run["phase_duration_ms"].values()))
        self.assertNotIn("private", json.dumps(report))

    def test_partial_node_spans_do_not_claim_uncovered_time_as_idle(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root, [attempt("partial", 1, 10)])
            path = journal(root, "partial", [])
            graph(path, "partial", 1, 4)
            checkpoint = json.loads((path / "graph" / "graph.json").read_text())
            checkpoint["nodes"].append({"id": "unclosed", "kind": "function", "status": "running", "started_at": at(2)})
            save(path / "graph" / "graph.json", checkpoint)
            report = analyze(root)
        run = report["graph_timing"]["per_run"]["partial"]
        self.assertEqual(run["status"], "partial")
        self.assertEqual(run["peak_concurrent_nodes"], 1)
        self.assertIsNone(run["uncovered_envelope_seconds"])

    def test_command_graph_is_separate_from_enclosing_agent_and_reuse_deduplicates(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root, [attempt("worker", 1, 10)])
            path = journal(root, "worker", [])
            graph(path, "worker", 1, 10)
            save(path / "graph-tools" / "parallel" / "graph.json", {
                "schema_version": 1, "version": "commands-v1", "run_id": "worker", "nodes": [
                    {"id": "left", "kind": "function", "status": "succeeded", "started_at": at(2), "finished_at": at(6), "run_duration_ms": 3900,
                     "output": {"value": {"stdout": "private output", "output_path": "private path"}}},
                    {"id": "right", "kind": "function", "status": "succeeded", "started_at": at(3), "finished_at": at(7)}]})
            graph_tool_call(path, "first", "parallel")
            graph_tool_call(path, "reuse", "parallel", 8, 9)
            report = analyze(root)
            graph_tool_call(path, "missing", "missing-checkpoint", 9, 10)
            partial = analyze(root)
        outer = report["graph_timing"]["node_intervals"]
        self.assertEqual(outer["cumulative_seconds"], 9)
        self.assertEqual(outer["active_wall_seconds"], 9)
        inner = report["tool_graph_timing"]
        self.assertEqual(inner["status"], "collected")
        self.assertEqual(inner["graphs"], 1)
        self.assertEqual(inner["tool_invocations"], 2)
        self.assertEqual(inner["node_intervals"]["cumulative_seconds"], 8)
        self.assertEqual(inner["node_intervals"]["active_wall_seconds"], 5)
        self.assertEqual(inner["per_graph"][0]["peak_concurrent_nodes"], 2)
        self.assertEqual(inner["per_graph"][0]["phase_duration_ms"]["run_duration_ms"]["count"], 1)
        self.assertNotIn("private", json.dumps(report))
        self.assertIn("两组时间不可相加", render(report))
        missing = partial["tool_graph_timing"]
        self.assertEqual(missing["status"], "partial")
        self.assertEqual(missing["graphs"], 2)
        self.assertEqual(missing["collected_graphs"], 1)
        unavailable = next(row for row in missing["per_graph"] if row["key"] == "missing-checkpoint")
        self.assertEqual(unavailable["checkpoint_status"], "not_collected")
        self.assertIsNone(unavailable["active_wall_seconds"])

    def test_command_graph_requires_matching_run_and_identifiable_tool_key(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root, [attempt("worker", 1, 10)])
            path = journal(root, "worker", [])
            save(path / "graph-tools" / "wrong" / "graph.json", {
                "schema_version": 1, "version": "commands-v1", "run_id": "another-worker", "nodes": []})
            graph_tool_call(path, "bad-key", "../private-command")
            report = analyze(root)
        inner = report["tool_graph_timing"]
        self.assertEqual(inner["status"], "not_collected")
        self.assertEqual(inner["unidentified_invocations"], 1)
        self.assertEqual(inner["per_graph"][0]["checkpoint_status"], "invalid_checkpoint")
        self.assertIsNone(inner["node_intervals"]["active_wall_seconds"])
        self.assertNotIn("private", json.dumps(report))

    def test_checkpoint_identity_and_parse_failure_are_not_measured(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            graph(root, "other-run", 1, 4)
            self.assertEqual(graph_observations(root, "expected")["status"], "invalid_checkpoint")
            graph(root, "expected", 1, 4)
            checkpoint = json.loads((root / "graph" / "graph.json").read_text())
            checkpoint["nodes"].append(checkpoint["nodes"][0].copy())
            save(root / "graph" / "graph.json", checkpoint)
            self.assertEqual(graph_observations(root, "expected")["status"], "invalid_checkpoint")
            (root / "graph" / "graph.json").write_text("incomplete JSON", encoding="utf-8")
            self.assertEqual(graph_observations(root, "expected")["status"], "invalid_checkpoint")

    def test_partial_role_coverage_is_explicit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root, [attempt("retained", 1, 4), attempt("missing", 5, 9)])
            path = journal(root, "retained", [(1, 3)])
            graph(path, "retained", 1, 4)
            missing = root / "workspace" / ".pwnmesh" / "runs" / "missing"
            save(missing / "job.json", {"kind": "explore", "intent": {"id": "missing-step"},
                                        "state": {"steps": [{"id": "missing-step"}]}})
            report = analyze(root)
        self.assertEqual(report["role_timing"]["explore"]["model"]["status"], "partial")
        self.assertEqual(report["graph_timing"]["status"], "partial")
        self.assertEqual(report["graph_timing"]["by_node"]["agent"]["status"], "partial")

    def test_curation_and_candidates_count_as_external_business_updates(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root, [attempt("reason", 1, 10, "reason")])
            journal(root, "reason", [(1, 9)], kind="reason")
            save(root / "state-events.json", [
                {"op": "candidate", "run_id": "live@worker", "revision": 1, "created_at": at(3)},
                {"op": "curate", "run_id": "live@curator", "revision": 2, "created_at": at(5)},
                {"op": "heartbeat", "run_id": "live@worker", "revision": 3, "created_at": at(6)},
                {"op": "step", "run_id": "live@reason", "revision": 4, "created_at": at(7)},
            ])
            report = analyze(root)
        self.assertEqual([row["op"] for row in report["external_business_updates_during_decisions"]], ["candidate", "curate"])

    def test_review_uses_the_matching_immutable_step_not_intent_or_other_steps(self):
        for key in ("input_view", "state"):
            job = {"intent": {"id": "ordinary", "dispute_id": "invented-field"},
                   key: {"steps": [{"id": "review", "dispute_id": "d1"}, {"id": "ordinary"}]}}
            self.assertEqual(execution_role(job, "explore", "review"), ("review", "immutable_job_" + key + "_step"))
            self.assertEqual(execution_role(job, "explore", "ordinary")[0], "explore")
            self.assertEqual(execution_role(job, "explore", "missing")[0], "execute_unknown")
        self.assertEqual(execution_role({"intent": {"id": "review", "dispute_id": "d1"}}, "explore", "review")[0], "execute_unknown")


if __name__ == "__main__":
    unittest.main()
