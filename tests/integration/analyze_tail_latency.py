"""Recompute completion-tail timing from retained receipts, never model prose."""
from __future__ import annotations

import argparse
from collections import Counter
import json
from pathlib import Path

import analyze_live_contention as base


def observed_anchor(event, runs, api_rows):
    """Bound acceptance using its transaction timestamp and successful receipt."""
    at = base.stamp(event.get("created_at"))
    if at is None:
        return None
    run_id, op = base.bare_run(event.get("run_id")), event["op"]
    spans = []
    if op == "step_completed":
        suffix = "/executions/" + run_id + "/apply"
        for row in api_rows:
            begin, duration = base.stamp(row.get("started")), row.get("duration_ms")
            if row.get("method") != "POST" or not row.get("path", "").endswith(suffix) or not 200 <= row.get("status", 0) < 300:
                continue
            if begin is not None and isinstance(duration, (int, float)) and duration >= 0:
                spans.append((begin, begin + duration / 1000))
    else:
        wanted = "commit" if op == "complete" else "curate"
        for run in runs:
            if base.bare_run(run.get("run_id")) != run_id:
                continue
            for tool in run.get("tools", []):
                if tool.get("op") != wanted or tool.get("failed"):
                    continue
                if op == "complete" and not any(item.get("committed") for item in tool.get("decision_operations", [])):
                    continue
                begin, end = base.stamp(tool.get("started")), base.stamp(tool.get("finished"))
                if begin is not None and end is not None and end >= begin:
                    spans.append((begin, end))
    # CreatedAt is the transaction's starting timestamp, not its durable commit.
    # A successful receipt bounds acceptance above, even if it is a replay of
    # an uncertain earlier commit. Its request start cannot raise the lower
    # bound, and the end of the event's rounded second is never a commit bound.
    spans = [(begin, end) for begin, end in spans if begin < at + 1 and end >= at]
    if len(spans) == 1:
        lower, upper = at, spans[0][1]
        basis = "transaction_timestamp_floor_and_unique_successful_receipt"
    else:
        lower, upper, basis = at, None, "transaction_timestamp_floor_without_unique_receipt"
    return {"op": op, "run_id": run_id, "revision": event.get("revision"),
            "lower": base.iso(lower), "upper": base.iso(upper), "basis": basis,
            "matching_receipts": len(spans)}


def window_metrics(runs, errors, window, expected_runs=()):
    calls, tools, selected_runs = [], [], []
    expected_runs = {base.bare_run(identity) for identity in expected_runs if identity}
    observed_runs = {base.bare_run(run.get("run_id")) for run in runs}
    missing_records = sorted(expected_runs - observed_runs)
    missing = set(missing_records)
    for run in runs:
        identity = base.bare_run(run.get("run_id"))
        begin, end = base.stamp(run.get("started")), base.stamp(run.get("finished"))
        # Missing bounds cannot establish that a run was outside the tail.
        relevant = identity in expected_runs or begin is None or end is None or begin < window[1] and end > window[0]
        if relevant:
            selected_runs.append(run)
            if "events.jsonl" in run.get("missing_evidence_files", []):
                missing.add(identity)
        for key, target in (("requests", calls), ("tools", tools)):
            for item in run.get(key, []):
                left, right = base.stamp(item.get("started")), base.stamp(item.get("finished"))
                if left is not None and right is not None and left < window[1] and right > window[0]:
                    target.append({"run_id": run.get("run_id"), "role": run.get("kind"), **item})
    model_union = base.merge_intervals([(base.stamp(x["started"]), base.stamp(x["finished"])) for x in calls], window)
    tool_union = base.merge_intervals([(base.stamp(x["started"]), base.stamp(x["finished"])) for x in tools], window)
    incomplete = sum(run.get("incomplete_model_observations", 0) for run in selected_runs)
    unpaired = sum(any(warning in {"unpaired_model_start", "model_end_without_start"} for warning in run.get("warnings", [])) for run in selected_runs)
    complete = bool(runs) and not missing and not incomplete and not unpaired
    model_seconds = base.interval_seconds(model_union)
    errors_in_window = [row for row in errors if window[0] <= row["at"] <= window[1]]
    return {"started": base.iso(window[0]), "finished": base.iso(window[1]),
            "wall_seconds": max(0, window[1] - window[0]),
            "model_active_union_seconds": model_seconds,
            "non_model_union_seconds": max(0, window[1] - window[0] - model_seconds) if complete else None,
            "tool_active_union_seconds": base.interval_seconds(tool_union),
            "model_calls_overlapping": len(calls), "failed_model_calls_overlapping": sum(bool(x.get("failed")) for x in calls),
            "model_calls_by_role": dict(Counter(x["role"] for x in calls)),
            "tool_calls_overlapping": len(tools), "failed_tool_calls_overlapping": sum(bool(x.get("failed")) for x in tools),
            "argument_retry_events": sum(x["kind"] == "tool_argument_retry" for x in errors_in_window),
            "argument_validation_failures": sum(x["kind"] == "argument_validation" for x in errors_in_window),
            "model_observation_status": "complete" if complete else "partial",
            "missing_journal_runs": sorted(missing), "missing_run_records": missing_records,
            "incomplete_model_observations": incomplete,
            "unpaired_model_journal_runs": unpaired}


def segment(start, end, runs, errors, state_events=()):
    if start is None or end is None:
        return None
    left, right = base.stamp(start["lower"]), base.stamp(end["lower"])
    start_upper, end_upper = base.stamp(start["upper"]), base.stamp(end["upper"])
    if end_upper is not None and end_upper < left:
        return None
    bounded = start_upper is not None and end_upper is not None
    # The journal inventory can itself be incomplete. Authoritative runtime
    # events prove these runs existed even when neither runs.json nor a retained
    # directory contains them. Include both anchors regardless of rounded time.
    expected_runs = {start.get("run_id"), end.get("run_id")}
    runtime_ops = base.BUSINESS_OPS | {"execution_failed", "dispute"}
    expected_runs.update(base.bare_run(event.get("run_id")) for event in state_events
                         if event.get("op") in runtime_ops
                         and start["revision"] <= event.get("revision", 0) <= end["revision"])
    return {"wall_seconds_lower_bound": max(0, right - start_upper) if bounded else None,
            "wall_seconds_upper_bound": max(0, end_upper - left) if bounded else None,
            "event_lower_bound_window": window_metrics(runs, errors, (left, max(left, right)), expected_runs),
            "event_window_basis": "Diagnostic window between transaction timestamp floors; these are not durable acceptance instants.",
            "enclosing_window": window_metrics(runs, errors, (left, max(left, end_upper)), expected_runs) if bounded else None}


def summarize(report, state_events, api_rows, errors):
    ordered = sorted(state_events, key=lambda row: row.get("revision", 0))
    completed = [row for row in ordered if row.get("op") == "complete" and base.stamp(row.get("created_at")) is not None]
    if not completed:
        return {"status": "unavailable", "reason": "no_authoritative_completion_event"}
    complete = completed[-1]
    before = [row for row in ordered if row.get("revision", 0) <= complete.get("revision", 0)]
    executes = [row for row in before if row.get("op") == "step_completed"]
    if not executes:
        return {"status": "unavailable", "reason": "no_accepted_execute_event"}
    execute = executes[-1]
    curations = [row for row in before if row.get("op") == "curate" and row.get("revision", 0) > execute.get("revision", 0)]
    runs = report.get("runs", [])
    anchors = {"last_accepted_execute": observed_anchor(execute, runs, api_rows),
               "last_post_execute_curation": observed_anchor(curations[-1], runs, api_rows) if curations else None,
               "project_complete": observed_anchor(complete, runs, api_rows)}
    if anchors["last_accepted_execute"] is None:
        return {"status": "unavailable", "reason": "accepted_execute_timestamp_missing"}
    bounded = anchors["last_accepted_execute"]["upper"] is not None and anchors["project_complete"]["upper"] is not None
    return {"status": "measured_with_bounds" if bounded else "diagnostic_only", "anchors": anchors,
            "execute_to_complete": segment(anchors["last_accepted_execute"], anchors["project_complete"], runs, errors, before),
            "execute_to_last_curation": segment(anchors["last_accepted_execute"], anchors["last_post_execute_curation"], runs, errors, before),
            "last_curation_to_complete": segment(anchors["last_post_execute_curation"], anchors["project_complete"], runs, errors, before),
            "curation_status": "observed_after_last_execute" if curations else "no_post_execute_curation_event",
            "basis": "Board CreatedAt records a second-resolution transaction-start timestamp, not its durable commit. Acceptance is no earlier than that timestamp floor and no later than a uniquely matching successful receipt's finish; absent or ambiguous receipts leave duration bounds and the enclosing acceptance window unknown. Request starts cannot raise the acceptance lower bound because a receipt may replay an earlier uncertain commit. The transaction-timestamp window is diagnostic, not a measured acceptance interval. Model and tool intervals are clipped unions, never summed concurrent durations. Calls count intervals that overlap the window, including requests that began earlier. Non-model time is unavailable for incomplete model journals or runtime runs missing from the retained inventory; otherwise it includes tools, scheduling, persistence, environment and unobserved work, not pure framework overhead. The two window estimates are not additive. No model text, tool arguments, paths, outputs or credentials are exported."}


def argument_events(output, runs):
    rows = []
    for run in runs:
        path = base.run_events_path(output, run["run_id"])
        if not path.exists():
            continue
        for line in path.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            event = json.loads(line)
            kind, error = event.get("type", "").removeprefix("replan_"), str(event.get("error") or "").lower()
            at = base.stamp(event.get("at"))
            if at is None:
                continue
            if kind == "tool_argument_retry":
                rows.append({"at": at, "kind": kind})
            elif kind == "tool_end" and (error.startswith(("tool arguments must be", "missing argument arguments", "unknown argument arguments")) or error.startswith("arguments") and " must " in error):
                rows.append({"at": at, "kind": "argument_validation"})
    return rows


def acceptance_receipt(validation, manifest, host):
    failures = []
    if validation.get("passed") is not True or validation.get("project_completed") is not True or validation.get("failures") != [] or manifest.get("acceptance_passed") is not True:
        failures.append("original_acceptance_not_passed")
    if type(host.get("test_exit_code")) is not int or host["test_exit_code"] != 0:
        failures.append("host_test_exit_zero_not_recorded")
    if "exit_code" in host and (type(host["exit_code"]) is not int or host["exit_code"] != 0):
        failures.append("host_exit_not_zero")
    source = manifest.get("source_commit")
    if not isinstance(source, str) or not source or host.get("source_commit") != source:
        failures.append("host_and_manifest_source_not_bound")
    if not validation.get("validation_scope") or validation.get("validation_scope") != manifest.get("validation_scope"):
        failures.append("validation_scope_not_bound")
    return {"passed": not failures, "failures": failures,
            "source_commit": source, "host_source_commit": host.get("source_commit"),
            "source_basis": "manifest.source_commit and host-run.source_commit must agree; no working-tree HEAD substitution"}


def analyze(output):
    output = Path(output)
    report = base.analyze(output)
    validation = base.read_json(output / "validation.json", {})
    manifest = base.read_json(output / "manifest.json", {})
    host = base.read_json(output / "host-run.json", {})
    acceptance = acceptance_receipt(validation, manifest, host)
    result = summarize(report, base.read_json(output / "state-events.json", []),
                       base.read_json(output / "api-observations.json", []), argument_events(output, report["runs"]))
    return {"version": 1, "source_commit": acceptance["source_commit"],
            "original_acceptance_passed": acceptance["passed"], "acceptance_receipt": acceptance,
            "validation_scope": validation.get("validation_scope"),
            "argument_failure_basis": "Explicit tool_argument_retry events plus failed tool_end messages matching runtime ValidateArguments prefixes. Semantic Board rejections and generic tool failures are not labelled argument failures; no model/tool content is exported.",
            **result}


def render(report):
    lines = ["# Completion tail observations", "", f"Source: `{report.get('source_commit')}`. Original acceptance: `{report['original_acceptance_passed']}`.", "",
             "Single-run timing describes this observation; it does not prove stable speedup or equal task quality.", ""]
    if report["status"] == "unavailable":
        return "\n".join(lines + ["Unavailable: " + report["reason"], ""])
    lines += ["| Interval | Wall bounds (s) | Model union (s) | Non-model union (s) | Model calls | Argument retries / validation failures |",
              "| --- | ---: | ---: | ---: | ---: | ---: |"]
    for key in ("execute_to_complete", "execute_to_last_curation", "last_curation_to_complete"):
        part = report[key]
        if part is None:
            lines.append(f"| {key} | unavailable | — | — | — | — |")
            continue
        metrics = part["event_lower_bound_window"]
        non_model = metrics["non_model_union_seconds"]
        shown = f"{non_model:.3f}" if non_model is not None else "unknown"
        bounds = (f"{part['wall_seconds_lower_bound']:.3f}–{part['wall_seconds_upper_bound']:.3f}"
                  if part["wall_seconds_lower_bound"] is not None and part["wall_seconds_upper_bound"] is not None else "unknown")
        lines.append(f"| {key} | {bounds} | {metrics['model_active_union_seconds']:.3f} | {shown} | {metrics['model_calls_overlapping']} | {metrics['argument_retry_events']} / {metrics['argument_validation_failures']} |")
    lines += ["", "Model/tool columns use the diagnostic transaction-timestamp window, not acceptance instants. Enclosing-window estimates are retained in JSON only when both acceptance anchors are bounded.", "", report["basis"], ""]
    return "\n".join(lines)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    result = analyze(args.output)
    (args.output / "tail-latency.json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
    (args.output / "tail-latency.md").write_text(render(result), encoding="utf-8")
    print(json.dumps({"report": str(args.output / 'tail-latency.md'), "status": result["status"]}))
