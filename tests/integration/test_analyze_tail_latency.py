import json
from pathlib import Path
import tempfile
import unittest

from analyze_tail_latency import acceptance_receipt, analyze, argument_events, observed_anchor, render, summarize, window_metrics


def at(second):
    return f"2026-09-29T00:00:{second:06.3f}Z"


def event(op, second, revision, run):
    return {"op": op, "created_at": at(second), "revision": revision, "run_id": "worker@" + run}


def run(identity, role, start, end, requests=(), tools=()):
    return {"run_id": identity, "kind": role, "started": at(start), "finished": at(end),
            "requests": list(requests), "tools": list(tools), "missing_evidence_files": [],
            "incomplete_model_observations": 0, "warnings": []}


def call(start, end, **fields):
    return {"started": at(start), "finished": at(end), **fields}


class TailLatencyTests(unittest.TestCase):
    def test_argument_events_read_legacy_archive_and_prefer_new_journal(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for brand, kind in ((".xloom", "tool_argument_retry"), (".pwnmesh", "tool_end")):
                journal = root / "workspace" / brand / "runs/a/events.jsonl"
                journal.parent.mkdir(parents=True)
                journal.write_text(json.dumps({"type": kind, "at": at(1),
                                                "error": "unknown argument arguments.PRIVATE"}), encoding="utf-8")
                rows = argument_events(root, [{"run_id": "a"}])
                self.assertEqual([row["kind"] for row in rows],
                                 ["tool_argument_retry" if brand == ".xloom" else "argument_validation"])
                self.assertNotIn("PRIVATE", json.dumps(rows))

    def test_original_acceptance_requires_explicit_host_success_and_bound_source(self):
        validation = {"passed": True, "project_completed": True, "failures": [], "validation_scope": "audit"}
        manifest = {"acceptance_passed": True, "source_commit": "original-source", "validation_scope": "audit"}
        host = {"source_commit": "original-source", "test_exit_code": 0}
        self.assertTrue(acceptance_receipt(validation, manifest, host)["passed"])
        for invalid in ({}, {"source_commit": "original-source"},
                        {**host, "test_exit_code": False}, {**host, "test_exit_code": 1},
                        {**host, "exit_code": 1}, {**host, "source_commit": "different-source"}):
            with self.subTest(host=invalid):
                self.assertFalse(acceptance_receipt(validation, manifest, invalid)["passed"])
        self.assertFalse(acceptance_receipt({**validation, "passed": False}, manifest, host)["passed"])
        self.assertFalse(acceptance_receipt(validation, {**manifest, "validation_scope": "changed"}, host)["passed"])

    def fixture(self):
        runs = [run("execute", "explore", 1, 10.2),
                run("curate", "curate", 10.1, 15.8, [call(11, 15)], [call(15.1, 15.7, op="curate")]),
                run("decide", "reason", 15.9, 20.5, [call(16, 20)],
                    [call(20.1, 20.4, op="commit", decision_operations=[{"committed": True}])])]
        events = [event("step_completed", 10, 3, "execute"), event("curate", 15, 4, "curate"), event("complete", 20, 5, "decide")]
        api = [{"method": "POST", "path": "/projects/p/executions/execute/apply", "status": 200,
                "started": at(10.1), "duration_ms": 100}]
        return runs, events, api

    def test_acceptance_is_authoritative_and_receipts_preserve_uncertainty(self):
        runs, events, api = self.fixture()
        # A later failed Runner finish is not an accepted Execute anchor.
        runs.append(run("failed", "explore", 10, 19, [call(10, 12, failed=True)]))
        result = summarize({"runs": runs}, events, api, [])
        anchor = result["anchors"]["last_accepted_execute"]
        self.assertEqual(anchor["run_id"], "execute")
        self.assertEqual(anchor["matching_receipts"], 1)
        self.assertAlmostEqual(result["execute_to_complete"]["wall_seconds_lower_bound"], 9.8, places=5)
        self.assertAlmostEqual(result["execute_to_complete"]["wall_seconds_upper_bound"], 10.4, places=5)
        self.assertEqual(result["curation_status"], "observed_after_last_execute")

    def test_transaction_longer_than_event_second_retains_full_receipt_upper_bound(self):
        from analyze_live_contention import stamp
        runs, events, api = self.fixture()
        api[0].update(started=at(10.8), duration_ms=1600)
        runs[0]["finished"] = at(12.5)
        runs[-1]["tools"][0].update(started=at(20.8), finished=at(22.4))
        runs[-1]["finished"] = at(22.5)
        result = summarize({"runs": runs}, events, api, [])
        anchor = result["anchors"]["last_accepted_execute"]
        self.assertEqual(stamp(anchor["lower"]), stamp(at(10)))
        self.assertAlmostEqual(stamp(anchor["upper"]), stamp(at(12.4)), places=5)
        part = result["execute_to_complete"]
        self.assertAlmostEqual(part["wall_seconds_lower_bound"], 7.6, places=5)
        self.assertAlmostEqual(part["wall_seconds_upper_bound"], 12.4, places=5)
        self.assertAlmostEqual(part["enclosing_window"]["wall_seconds"], 12.4, places=5)

    def test_replayed_success_does_not_move_acceptance_after_request_start(self):
        from analyze_live_contention import stamp
        runs, events, api = self.fixture()
        # A prior uncertain commit can precede the only successful replay.
        api[0].update(started=at(10.8), duration_ms=100)
        anchor = observed_anchor(events[0], runs, api)
        self.assertEqual(stamp(anchor["lower"]), stamp(at(10)))
        self.assertAlmostEqual(stamp(anchor["upper"]), stamp(at(10.9)), places=5)

    def test_concurrent_calls_are_unioned_and_clipped(self):
        from analyze_live_contention import stamp
        runs = [run("a", "curate", 1, 20, [call(8, 15)]),
                run("b", "reason", 1, 20, [call(12, 18)])]
        metrics = window_metrics(runs, [], (stamp(at(10)), stamp(at(20))))
        self.assertEqual(metrics["model_calls_overlapping"], 2)
        self.assertEqual(metrics["model_active_union_seconds"], 8)
        self.assertEqual(metrics["non_model_union_seconds"], 2)

    def test_missing_or_unpaired_journals_never_become_non_model_time(self):
        from analyze_live_contention import stamp
        for patch in ({"missing_evidence_files": ["events.jsonl"]},
                      {"incomplete_model_observations": 1},
                      {"warnings": ["model_end_without_start"]}):
            with self.subTest(patch=patch):
                entry = run("a", "reason", 1, 20, [call(11, 12)])
                entry.update(patch)
                result = window_metrics([entry], [], (stamp(at(10)), stamp(at(20))))
                self.assertIsNone(result["non_model_union_seconds"])
                self.assertEqual(result["model_observation_status"], "partial")

    def test_authoritative_anchor_runs_missing_from_inventory_are_partial(self):
        for absent in ("execute", "curate", "decide"):
            with self.subTest(absent=absent):
                runs, events, api = self.fixture()
                runs = [entry for entry in runs if entry["run_id"] != absent]
                result = summarize({"runs": runs}, events, api, [])
                for window in ("event_lower_bound_window", "enclosing_window"):
                    metrics = result["execute_to_complete"][window]
                    if metrics is None:
                        continue
                    self.assertEqual(metrics["missing_run_records"], [absent])
                    self.assertEqual(metrics["missing_journal_runs"], [absent])
                    self.assertEqual(metrics["model_observation_status"], "partial")
                    self.assertIsNone(metrics["non_model_union_seconds"])
                # Missing final Decide evidence does not invalidate an earlier
                # fully observed Execute-to-Curate segment, and vice versa.
                unaffected = {"execute": "last_curation_to_complete", "decide": "execute_to_last_curation"}.get(absent)
                if unaffected:
                    self.assertEqual(result[unaffected]["event_lower_bound_window"]["model_observation_status"], "complete")

    def test_non_anchor_runtime_events_also_require_a_retained_run(self):
        runs, events, api = self.fixture()
        events[1]["revision"], events[2]["revision"] = 5, 6
        events.append(event("candidate", 13, 4, "missing-producer"))
        events.append(event("fact", 5, 1, "older-run-outside-tail"))
        result = summarize({"runs": runs}, events, api, [])
        for part in ("execute_to_complete", "execute_to_last_curation"):
            metrics = result[part]["event_lower_bound_window"]
            self.assertEqual(metrics["missing_run_records"], ["missing-producer"])
            self.assertIsNone(metrics["non_model_union_seconds"])
        self.assertEqual(result["last_curation_to_complete"]["event_lower_bound_window"]["model_observation_status"], "complete")

    def test_missing_run_and_directory_are_detected_through_real_analyzer(self):
        runs, events, api = self.fixture()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            def save(name, value):
                (root / name).write_text(json.dumps(value), encoding="utf-8")
            save("manifest.json", {"started": at(0), "completed_observed": at(21), "source_commit": "original"})
            save("runs.json", runs[:1])
            save("state-events.json", events)
            save("api-observations.json", api)
            directory = root / "workspace/.pwnmesh/runs/execute"
            directory.mkdir(parents=True)
            (directory / "job.json").write_text(json.dumps({"run_id": "execute", "kind": "explore"}), encoding="utf-8")
            (directory / "session.json").write_text("{}", encoding="utf-8")
            (directory / "events.jsonl").write_text("", encoding="utf-8")
            result = analyze(root)
        self.assertEqual(result["status"], "diagnostic_only")
        metrics = result["execute_to_complete"]["event_lower_bound_window"]
        self.assertEqual(metrics["missing_run_records"], ["curate", "decide"])
        self.assertEqual(metrics["model_observation_status"], "partial")
        self.assertIsNone(metrics["non_model_union_seconds"])
        self.assertIsNone(result["execute_to_complete"]["enclosing_window"])

    def test_ambiguous_receipt_replays_do_not_invent_precision(self):
        runs, events, api = self.fixture()
        api.append({**api[0], "started": at(10.4)})
        anchor = observed_anchor(events[0], runs, api)
        self.assertEqual(anchor["basis"], "transaction_timestamp_floor_without_unique_receipt")
        self.assertEqual(anchor["matching_receipts"], 2)
        self.assertIsNone(anchor["upper"])

    def test_missing_or_ambiguous_receipts_leave_duration_bounds_unknown(self):
        for ambiguous in (False, True):
            with self.subTest(ambiguous=ambiguous):
                runs, events, api = self.fixture()
                api = api + [{**api[0], "started": at(10.4)}] if ambiguous else []
                result = summarize({"runs": runs}, events, api, [])
                self.assertEqual(result["status"], "diagnostic_only")
                part = result["execute_to_complete"]
                self.assertIsNone(part["wall_seconds_lower_bound"])
                self.assertIsNone(part["wall_seconds_upper_bound"])
                self.assertIsNone(part["enclosing_window"])
                self.assertEqual(part["event_lower_bound_window"]["wall_seconds"], 10)
                self.assertIn("Diagnostic", part["event_window_basis"])
                text = render({"source_commit": "test", "original_acceptance_passed": False, **result})
                self.assertIn("| execute_to_complete | unknown |", text)

    def test_failed_commit_cannot_refine_completed_event(self):
        runs, events, api = self.fixture()
        runs[-1]["tools"][0]["failed"] = True
        anchor = observed_anchor(events[-1], runs, api)
        self.assertEqual(anchor["basis"], "transaction_timestamp_floor_without_unique_receipt")
        self.assertIsNone(anchor["upper"])

    def test_no_completion_remains_unavailable_even_with_finished_workers(self):
        runs, events, api = self.fixture()
        self.assertEqual(summarize({"runs": runs}, events[:-1], api, []),
                         {"status": "unavailable", "reason": "no_authoritative_completion_event"})

    def test_earlier_curation_is_not_mislabelled_as_tail_curation(self):
        runs, events, api = self.fixture()
        events[1] = event("curate", 8, 2, "curate")
        result = summarize({"runs": runs}, events, api, [])
        self.assertIsNone(result["last_curation_to_complete"])
        self.assertEqual(result["curation_status"], "no_post_execute_curation_event")

    def test_same_second_events_keep_revision_order_without_invented_bounds(self):
        runs = []
        events = [event("complete", 10, 5, "decide"), event("step_completed", 10, 3, "execute"), event("curate", 10, 4, "curate")]
        result = summarize({"runs": runs}, events, [], [])
        self.assertIsNone(result["execute_to_complete"]["wall_seconds_lower_bound"])
        self.assertIsNone(result["execute_to_complete"]["wall_seconds_upper_bound"])
        self.assertIsNone(result["execute_to_complete"]["enclosing_window"])
        self.assertEqual(result["execute_to_complete"]["event_lower_bound_window"]["wall_seconds"], 0)

    def test_argument_events_export_counts_without_error_text(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            journal = root / "workspace/.pwnmesh/runs/a/events.jsonl"
            journal.parent.mkdir(parents=True)
            journal.write_text("\n".join(json.dumps(item) for item in [
                {"type": "tool_argument_retry", "at": at(11), "error": "PRIVATE_JSON"},
                {"type": "tool_end", "at": at(12), "error": "unknown argument arguments.PRIVATE_ARGUMENT"},
                {"type": "tool_end", "at": at(13), "error": "timeout PRIVATE_COMMAND"},
                {"type": "tool_end", "at": at(13), "error": "candidate sources must be valid PRIVATE_DATA"},
                {"type": "replan_tool_argument_retry", "at": at(14), "error": "PRIVATE_JSON"},
            ]), encoding="utf-8")
            rows = argument_events(root, [{"run_id": "a"}])
        self.assertEqual([row["kind"] for row in rows], ["tool_argument_retry", "argument_validation", "tool_argument_retry"])
        self.assertNotIn("PRIVATE", json.dumps(rows))

    def test_render_keeps_sample_and_measurement_limits(self):
        runs, events, api = self.fixture()
        report = {"source_commit": "test", "original_acceptance_passed": False,
                  **summarize({"runs": runs}, events, api, [])}
        text = render(report)
        self.assertIn("does not prove stable speedup", text)
        self.assertIn("not pure framework overhead", text)
        self.assertIn("False", text)


if __name__ == "__main__":
    unittest.main()
