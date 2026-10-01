"""Offline efficiency accounting; retain counts/hashes, never model or tool text."""
from __future__ import annotations

import argparse
from collections import Counter, defaultdict
import hashlib
import json
import math
from pathlib import Path
from statistics import median
from urllib.parse import urlsplit

import analyze_live_contention as base


BYTE_FIELDS = ("body", "system", "tools", "messages", "thinking_blocks", "tool_result_blocks",
               "text_blocks", "tool_use_blocks", "other_blocks", "repeated_message_bytes")
READ_TOOLS = {"read", "grep", "find", "ls", "read_graph", "read_snapshot"}
TOOLS = READ_TOOLS | {"bash", "edit", "write", "graph_action", "cvss31"}
SECTIONS = {"overview", "facts", "goals", "steps", "findings", "relations", "hints", "evidence", "sources"}
OPS = {"fact", "finding", "goal", "step", "fact_relation", "complete", "preview", "commit", "reset"}


def fraction(numerator, denominator):
    return numerator / denominator if numerator is not None and denominator else None


def input_composition(rows):
    """Count HTTP transmissions independently from application usage/token totals."""
    seen, unique, duplicates = set(), [], 0
    for row in rows:
        key = (row.get("run_id"), row.get("request_id"))
        if key[1] is not None and key in seen:
            duplicates += 1
            continue
        if key[1] is not None:
            seen.add(key)
        unique.append(row)

    def summarize(items):
        measured = [row["input_bytes"] for row in items
                    if isinstance(row.get("input_bytes"), dict) and row["input_bytes"].get("body", 0) > 0]
        totals, coverage = {}, {}
        for key in BYTE_FIELDS:
            values = [item[key] for item in measured if isinstance(item.get(key), (int, float)) and item[key] >= 0]
            totals[key], coverage[key] = (sum(values) if values else None), len(values)
        return {"requests": len(items), "measured_requests": len(measured), "byte_totals": totals,
                "field_coverage": coverage,
                "status": "reported" if measured and len(measured) == len(items) else "partial" if measured else "unknown",
                "repeated_history_fraction_of_messages": fraction(totals["repeated_message_bytes"], totals["messages"]) if coverage["repeated_message_bytes"] == coverage["messages"] == len(measured) else None,
                "repeated_history_fraction_of_body": fraction(totals["repeated_message_bytes"], totals["body"]) if coverage["repeated_message_bytes"] == coverage["body"] == len(measured) else None}

    by_run = defaultdict(list)
    for row in unique:
        by_run[row.get("run_id") or "unknown"].append(row)
    return {**summarize(unique), "duplicate_observation_rows_excluded": duplicates,
            "by_run": {run: summarize(items) for run, items in sorted(by_run.items())},
            "basis": "Serialized UTF-8 JSON bytes, NOT tokens. Components overlap: blocks are inside messages, inside body. Repeated bytes are entire identical messages previously transmitted within the same run; denominator messages includes JSON array punctuation. Retries are transmissions too. This does not prove uncached billing or unnecessary work."}


def normalize_api_path(path):
    parts = urlsplit(path or "/").path.strip("/").split("/")
    replacements = {"projects": "{pid}", "intents": "{iid}", "rounds": "{generation}", "entries": "{entry}"}
    for i in range(1, len(parts)):
        parent = parts[i - 1]
        if parent in replacements:
            parts[i] = replacements[parent]
        elif parent == "executions" and parts[i] not in {"pending", "prepare", "check"}:
            parts[i] = "{rid}"
    return "/" + "/".join(parts)


def api_category(method, path):
    # These endpoints are polled/exported by this experiment's harness. Their
    # costs must not be attributed to the production scheduler or workers.
    if method == "GET" and path in {"/projects/{pid}/state", "/projects/{pid}/state/events", "/projects/{pid}/export"}:
        return "observer_" + {"state": "state_poll", "events": "event_export", "export": "export"}[path.rsplit("/", 1)[-1]]
    if path.endswith("/heartbeat"):
        return "heartbeat"
    if path.endswith("/updates"):
        return "dependency_updates"
    if path.endswith("/input/read"):
        return "snapshot_read"
    if "/state/decisions/" in path:
        return "decision_" + path.rsplit("/", 1)[-1]
    if path.endswith("/state/actions"):
        return "graph_write"
    if path.endswith(("/state/read", "/state", "/state/events")):
        return "graph_read"
    if path.endswith("/observation"):
        return "execution_observation"
    if path.endswith(("/scheduling", "/pending", "/check", "/identity")):
        return "scheduling"
    return "execution_control" if "/executions" in path else "project_control"


def api_observations(rows, window, collected):
    def summarize(items):
        return {"calls": len(items), "failed_calls": sum((row.get("status") or 0) >= 400 for row in items),
                "unknown_status_calls": sum(not row.get("status") for row in items),
                "duration_ms": base.stats(row.get("duration_ms") for row in items),
                "response_bytes": base.stats(row.get("response_bytes") for row in items)}
    routes, categories = defaultdict(list), defaultdict(list)
    intervals, production_intervals, observer_intervals = [], [], []
    included, production, observer = [], [], []
    outside, unknown_time = 0, 0
    for row in rows:
        start, duration = base.stamp(row.get("started")), row.get("duration_ms")
        if start is None:
            unknown_time += 1
            continue
        if not window[0] <= start < window[1]:
            outside += 1
            continue
        row = dict(row)
        if duration is not None and duration >= 0:
            row["duration_ms"] = min(duration, (window[1] - start) * 1000)
        path = normalize_api_path(row.get("path"))
        method = row.get("method") or "UNKNOWN"
        category = api_category(method, path)
        routes[method + " " + path].append(row)
        categories[category].append(row)
        included.append(row)
        (observer if category.startswith("observer_") else production).append(row)
        if start is not None and duration is not None and duration >= 0:
            interval = (start, start + duration / 1000)
            intervals.append(interval)
            (observer_intervals if category.startswith("observer_") else production_intervals).append(interval)
    active = lambda values: base.interval_seconds(base.merge_intervals(values, window)) if collected else None
    return {"status": "collected" if collected else "not_collected", **summarize(included),
            "excluded_outside_project_window": outside, "excluded_unknown_timestamp": unknown_time,
            "production": {**summarize(production), "active_wall_seconds": active(production_intervals)},
            "observer": {**summarize(observer), "active_wall_seconds": active(observer_intervals)},
            "active_wall_seconds": base.interval_seconds(base.merge_intervals(intervals, window)) if collected else None,
            "by_route": {key: summarize(value) for key, value in sorted(routes.items())},
            "by_category": {key: summarize(value) for key, value in sorted(categories.items())},
            "basis": "Only calls starting inside the project window are included; durations are clipped at its end. Response bytes are whole responses for those calls. Server handler time includes serialization and store/lock work, not pure SQLite time. GET project state/state-events/export are harness observer costs, separate from production. Durations are cumulative; active_wall_seconds is an interval union. API/tool/model and production/observer intervals can overlap and must not be added. Rows have no unique request ID, so identical-looking calls are not deduplicated."}


def tool_signatures(path):
    """Discard content immediately; only tool-use input hashes leave this parser."""
    signatures = {}
    if not path.exists():
        return signatures
    with path.open(encoding="utf-8-sig") as source:
        for line in source:
            if not line.strip():
                continue
            event = json.loads(line)
            kind = event.get("type", "")
            if kind not in {"message_end", "replan_message_end"}:
                continue
            message = event.get("message") or {}
            if message.get("role") != "assistant":
                continue
            for block in message.get("content", []):
                if block.get("type") != "tool_use":
                    continue
                args = block.get("input") or {}
                encoded = json.dumps(args, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()
                signatures[("replan" if kind.startswith("replan_") else "main", block.get("id"))] = hashlib.sha256(encoded).hexdigest()
    return signatures


def tool_analysis(runs, output):
    groups, fingerprints = defaultdict(list), []
    missing_inputs, repeated, repeated_successful = 0, 0, 0
    for run in runs:
        signatures = tool_signatures(base.run_events_path(output, run["run_id"]))
        seen, successful = set(), set()
        for tool in run["tools"]:
            name = tool.get("name") if tool.get("name") in TOOLS else "unknown"
            selector = tool.get("op") if name == "graph_action" else tool.get("section") if name in {"read_graph", "read_snapshot"} else None
            allowed = OPS if name == "graph_action" else SECTIONS
            label = name + ":" + (selector if selector in allowed else "unknown") if name in {"graph_action", "read_graph", "read_snapshot"} else name
            groups[label].append(tool)
            if name not in READ_TOOLS:
                continue
            stream = tool.get("stream", "main")
            digest = signatures.get((stream, tool.get("tool_id")))
            if digest is None:
                missing_inputs += 1
                continue
            key = (stream, name, digest)
            is_repeat = key in seen
            after_success = key in successful
            repeated += is_repeat
            repeated_successful += after_success
            fingerprints.append({"run_id": run["run_id"], "tool_id": tool.get("tool_id"), "tool": label,
                                 "input_sha256": digest, "repeated": is_repeat, "previous_success": after_success})
            seen.add(key)
            if tool.get("finished") and not tool.get("failed"):
                successful.add(key)
    return {"by_operation": {key: {"calls": len(items), "failed_calls": sum(bool(tool.get("failed")) for tool in items),
                                    "incomplete_calls": sum(not tool.get("finished") for tool in items),
                                    "duration_ms": base.stats(tool.get("duration_ms") for tool in items),
                                    "error_kinds": dict(Counter(tool.get("error_kind") or "unknown" for tool in items if tool.get("failed")))}
                             for key, items in sorted(groups.items())},
            "same_read_input_repeats": repeated, "same_read_input_after_previous_success": repeated_successful,
            "reads_without_input_metadata": missing_inputs, "read_fingerprints": fingerprints,
            "basis": "Executed tool_start observations; assistant/tool-result copies do not add calls. Read identity is canonical JSON input + tool name within run and main/shadow stream. Repeated reads may be required after state changes; no tool arguments, commands, results or thinking are exported."}


def execution_analysis(rows, runs, collected):
    # The scheduler's terminal result survives cancellation even when the
    # worker's session/result and Runner result are empty. Keep both sources.
    selected = [{"run_id": row.get("id"), "kind": row.get("kind"),
                 "status": row.get("status"),
                 "failure_kind": (row.get("result") or {}).get("failure_kind")}
                for row in rows]
    stale = [row for row in selected if row["kind"] in {"reason", "curate"} and row["status"] == "failed"
             and row["failure_kind"] == "state_changed"]
    stale_ids = {row["run_id"] for row in stale}
    matched = [run for run in runs if run["run_id"] in stale_ids]
    calls = [call for run in matched for call in run["requests"]]
    return {"status": "collected" if collected else "not_collected", "executions": selected,
            "state_changed_decision_runs": len(stale) if collected else None,
            "state_changed_decision_run_ids": sorted(stale_ids),
            "state_changed_decision_model_calls": len(calls) if collected else None,
            "state_changed_decision_model_duration_ms": sum(call.get("duration_ms") or 0 for call in calls) if collected else None,
            "state_changed_decision_usage": base.usage_totals(calls) if collected else None,
            "state_changed_decisions_without_run_evidence": len(stale_ids - {run["run_id"] for run in matched}) if collected else None,
            "basis": "executions.json terminal reason/curate results with status=failed and failure_kind=state_changed; separate from observed tool conflicts. Matched run model calls/usage are subsets of existing totals, never additional cost. Cancellation does not prove all prior work was useless."}


def apply_time_scope(report, manifest, output):
    report["observed_activity_window_seconds"] = report["project_wall_seconds"]
    report["observed_activity_finished"] = report["finished"]
    completed = report["end_basis"] in {"authoritative_complete_state_event", "completed_observed_poll",
                                         "successful_complete_commit_receipt_observed"}
    report["project_completed"] = completed
    report["time_window_scope"] = "completed_project" if completed else "observed_activity_only"
    report["terminal_status_observed_at"] = None
    report["time_to_terminal_observation_seconds"] = None
    report["terminal_observation_basis"] = None
    if completed:
        return
    # Last run finish is not project stop time. Do not invent an end timestamp
    # from a stale run or a final status without an associated observation time.
    report["project_wall_seconds"], report["finished"] = None, None
    report["timing"]["basis"] += "; incomplete project: observed activity window only, not project completion or total elapsed time"
    state = base.read_json(output / "state.json", {})
    status = (state.get("graph") or {}).get("project", {}).get("status")
    progress = base.read_json(output / "progress.json", {})
    observed, basis = None, None
    if status in {"stopped", "failed", "deleted"}:
        observed = base.stamp(manifest.get("stopped_observed"))
        if observed is not None:
            basis = "manifest_stopped_observed"
        elif (progress.get("progress") or "").split(" ", 1)[0] == "status=" + status:
            observed = base.stamp(progress.get("at"))
            basis = "terminal_status_progress_poll" if observed is not None else None
    started = base.stamp(report["started"])
    if observed is not None and observed >= started:
        report["terminal_status_observed_at"] = base.iso(observed)
        report["time_to_terminal_observation_seconds"] = observed - started
        report["terminal_observation_basis"] = basis


def mark_missing_http_observations(report):
    report["http_observation_status"] = "not_collected"
    for key in ("http_attempt_count", "proxy_error_class_counts", "http_retries_in_matched_logical_calls",
                "unmatched_http_attempts", "http_timing_ms", "http_stage_cumulative_seconds",
                "thinking_chars", "output_chars", "proxy_reported_usage", "usage_difference_calls"):
        report[key] = None
    report["usage_comparison_basis"] = "HTTP observation file is absent; HTTP counts, retries, timing and usage are unknown. Application model_call_end usage remains available."


def analyze(output):
    output = Path(output)
    report = base.analyze(output)
    manifest = base.read_json(output / "manifest.json", {})
    report["validation_scope"] = report["business_validation"].get("validation_scope") or manifest.get("validation_scope") or "unspecified"
    report["workload_context"] = {key: manifest[key] for key in ("workload_title", "scope", "study_role", "study_notes") if key in manifest}
    apply_time_scope(report, manifest, output)
    if not (output / "http-observations.json").exists():
        mark_missing_http_observations(report)
    report["input_composition"] = input_composition(base.read_json(output / "http-observations.json", []) or [])
    api_path = output / "api-observations.json"
    report["api_observations"] = api_observations(base.read_json(api_path, []) or [],
                                                (base.stamp(report["started"]), base.stamp(report["observed_activity_finished"])), api_path.exists())
    execution_path = output / "executions.json"
    report["execution_analysis"] = execution_analysis(base.read_json(execution_path, []) or [], report["runs"], execution_path.exists())
    report["tool_analysis"] = tool_analysis(report["runs"], output)
    report["run_costs"] = [{key: run.get(key) for key in ("run_id", "kind", "status", "model_calls", "summary_calls", "failed_model_calls", "model_duration_ms", "tool_duration_ms", "reported_usage", "usage_calls", "usage_status", "repair_count")}
                           for run in report["runs"]]
    report["efficiency_basis"] = "Base timing/usage comes from analyze_live_contention.analyze unchanged. Application model_call_end usage is authoritative for this report, not a bill; proxy usage is separately reported and never added. Missing observations are unknown. New input composition values are bytes, not tokens."
    return report


def render(report):
    comp, api, tools = report["input_composition"], report["api_observations"], report["tool_analysis"]
    ratio = comp["repeated_history_fraction_of_messages"]
    validation_label = "结构/报告交付验证" if report["validation_scope"] == "structural_report_delivery_only" else "已配置验证"
    # The unchanged base renderer expects a numeric timing window. Give it the
    # activity window, then label every project-duration reference accordingly.
    rendering = dict(report, project_wall_seconds=report["observed_activity_window_seconds"])
    base_text = base.render(rendering).replace("业务验收", validation_label).replace("timing-report.json", "efficiency-report.json")
    if not report["project_completed"]:
        base_text = base_text.replace("项目墙钟", "观测活动窗口")
        base_text = base_text.replace("## 时间归因", "项目未完成；完整项目耗时未知。以下窗口截止最后有结束时间的 run，不代表完成或停止时刻。\n\n## 观测活动窗口内的时间归因")
        elapsed = report["time_to_terminal_observation_seconds"]
        if elapsed is not None:
            base_text += f"\n启动至轮询观察到终止状态为 {elapsed:.3f} 秒（{report['terminal_observation_basis']}）；该时间包含最后 run 结束后的等待，不能当作成功任务耗时或精确停止时刻。\n"
    if report["http_observation_status"] == "not_collected" and report["http_observation_mode"] != "direct":
        base_text = base_text.replace("HTTP 观测未采集（direct 直连）", "HTTP 观测文件缺失")
        base_text = base_text.replace("direct 模式下模型请求未经过观测代理，未采集", "HTTP 观测文件缺失，无法确认")
    executions = report["execution_analysis"]
    count = executions["state_changed_decision_runs"]
    execution_text = (f"执行记录中的 state_changed 决策取消：{count} 个 run；对应已观测模型调用 {executions['state_changed_decision_model_calls']} 次，累计 {executions['state_changed_decision_model_duration_ms'] / 1000:.3f} 秒。"
                      if count is not None else "执行记录未采集，调度层 state_changed 决策取消次数未知。")
    lines = [base_text, "## 输入组成与黑板 API", "",
             execution_text + " 此处独立于工具返回的 state_changed 计数；对应调用和 usage 已包含在总量中，不可再相加，也不证明这些 run 的全部工作无用。",
             "HTTP 失败响应中未返回的 usage 仍为未知；代理固定数值字段中的 0 不能证明 HTTP 402 等失败尝试免费。",
             f"验证范围：`{report['validation_scope']}`。结构或报告交付检查不证明漏洞覆盖完整、发现语义正确或任务质量达标。耗时也包含靶场/环境排查与模型决策成本，不能全部归因于 PwnMesh 本体。",
             "输入组成全部按序列化 UTF-8 JSON 字节统计，不是 token；消息块、messages 和 body 为嵌套口径，不能相加。历史重传可能命中缓存，不能直接算作浪费。",
             f"有组成数据的 HTTP 请求：{comp['measured_requests']} / {comp['requests']}；相同 run 历史消息重传占 messages：{ratio:.1%}。" if ratio is not None else "请求输入组成或历史重传比例未知。",
             "", "| 输入部分 | 累计字节 |", "| --- | ---: |"]
    lines += [f"| {key} | {value if value is not None else '未知'} |" for key, value in comp["byte_totals"].items()]
    lines += ["", "| API 类别 | 次数 | 失败 | 累计毫秒 | p95 毫秒 | 响应字节 |", "| --- | ---: | ---: | ---: | ---: | ---: |"]
    for key, row in api["by_category"].items():
        duration = row["duration_ms"]
        lines.append(f"| {key} | {row['calls']} | {row['failed_calls']} | {duration.get('sum', '未知')} | {duration.get('p95', '未知')} | {row['response_bytes'].get('sum', '未知')} |")
    lines += ["", "API 是项目窗口内的服务端 handler 耗时，包含序列化、锁和存储操作，不能标为纯 SQLite 耗时；它与工具区间重叠，不能加到项目墙钟。observer 类别为实验轮询/导出，单列并排除于 production；窗口外调用未计入。缺失 API 文件表示未采集。",
              f"相同 run/工具/输入的重复读取 {tools['same_read_input_repeats']} 次，其中此前已有成功读取 {tools['same_read_input_after_previous_success']} 次；状态更新后的重读可能必要。",
              "各 run 用量、按 reason/explore 分组、API 路由与工具操作细分见 efficiency-report.json。", ""]
    return "\n".join(lines)


COMPARISON_KEYS = ("comparison_protocol_version", "workload_sha256", "execution_knobs_sha256", "validation_contract_sha256",
                   "model", "upstream", "reasoning_effort", "orchestration_version",
                   "http_observation_mode", "validation_scope")


def comparison_run(directory):
    """Read original receipts only; a later reviewed PASS cannot replace a FAIL."""
    directory = Path(directory).resolve()
    manifest = base.read_json(directory / "manifest.json", {})
    validation = base.read_json(directory / "validation.json", {})
    # Hash frozen validator source independently of the original timing and
    # acceptance receipts. A scope name alone cannot identify its semantics.
    contract = base.read_json(directory / "comparison-contract.json", {})
    controlled = {key: manifest.get(key) for key in COMPARISON_KEYS}
    controlled["validation_contract_sha256"] = contract.get("validation_contract_sha256")
    errors = []
    for key in COMPARISON_KEYS:
        value = controlled[key]
        if value is None or value == "":
            errors.append("missing " + key)
    if type(manifest.get("comparison_protocol_version")) is not int or manifest["comparison_protocol_version"] != 1:
        errors.append("unsupported comparison_protocol_version")
    for key in ("workload_sha256", "execution_knobs_sha256", "validation_contract_sha256"):
        value = controlled[key]
        if not isinstance(value, str) or len(value) != 64 or any(c not in "0123456789abcdef" for c in value):
            errors.append("invalid " + key)
    for key in ("source_commit", "namespace", "project_id", "started"):
        if not isinstance(manifest.get(key), str) or not manifest[key]:
            errors.append("missing " + key)
    if validation.get("validation_scope") != manifest.get("validation_scope"):
        errors.append("original validation_scope does not match manifest")

    def timestamp(key):
        try:
            return base.stamp(manifest.get(key))
        except (ValueError, TypeError, AttributeError):
            return None

    def duration(value):
        return value if isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value) and value > 0 else None

    accepted = (validation.get("passed") is True and validation.get("project_completed") is True
                and validation.get("failures") == [] and manifest.get("acceptance_passed") is True)
    # Optional host exit metadata can disqualify an otherwise successful receipt.
    host = base.read_json(directory / "host-run.json", {})
    if any(key in host and host[key] != 0 for key in ("exit_code", "test_exit_code")):
        accepted = False
    elapsed = duration(manifest.get("time_to_acceptance_seconds"))
    started, completed, finished = (timestamp(key) for key in ("started", "completed_observed", "validation_finished"))
    timed = (accepted and elapsed is not None and started is not None and completed is not None
             and finished is not None and started <= completed <= finished
             and elapsed + 0.001 >= completed - started
             and abs(elapsed - (finished - started)) <= 0.01)
    return {"directory": str(directory), "source_commit": manifest.get("source_commit"),
            "identity": [manifest.get("namespace"), manifest.get("project_id"), manifest.get("started")],
            "match": controlled,
            "status": "PASS" if accepted else "FAIL_OR_UNVERIFIED", "acceptance_passed": accepted,
            "time_to_acceptance_seconds": elapsed if timed else None,
            "elapsed_to_validation_seconds": elapsed,
            "timing_status": "measured" if timed else "unavailable" if accepted else "excluded_failed_or_unverified",
            "failure_count": len(validation["failures"]) if isinstance(validation.get("failures"), list) else None,
            "metadata_errors": errors}


def compare_runs(baseline, candidate):
    if not baseline or not candidate:
        raise ValueError("comparison requires both baseline and candidate runs")
    groups = {"baseline": [comparison_run(path) for path in baseline],
              "candidate": [comparison_run(path) for path in candidate]}
    expected = groups["baseline"][0]["match"]
    errors, identities, directories = [], set(), set()
    for label, rows in groups.items():
        sources = {row["source_commit"] for row in rows}
        if len(sources) != 1:
            errors.append(label + ": source_commit differs within group")
        for index, row in enumerate(rows, 1):
            prefix = f"{label}[{index}]: "
            errors.extend(prefix + error for error in row["metadata_errors"])
            errors.extend(prefix + "mismatched " + key for key in COMPARISON_KEYS if row["match"][key] != expected[key])
            identity = tuple(row["identity"])
            if row["directory"] in directories or identity in identities:
                errors.append(prefix + "duplicate run; copied evidence is not another sample")
            directories.add(row["directory"])
            identities.add(identity)
    matched = not errors
    summaries = {}
    for label, rows in groups.items():
        values = [row["time_to_acceptance_seconds"] for row in rows if row["time_to_acceptance_seconds"] is not None] if matched else []
        summaries[label] = {"attempts": len(rows), "original_passes": sum(row["acceptance_passed"] for row in rows),
                            "timed_passes": len(values), "median_seconds": median(values) if values else None,
                            "min_seconds": min(values) if values else None, "max_seconds": max(values) if values else None}
    sufficient = matched and all(summary["timed_passes"] >= 3 for summary in summaries.values())
    before, after = (summaries[key]["median_seconds"] for key in ("baseline", "candidate"))
    reduction = before - after if before is not None and after is not None else None
    return {"matched": matched, "matching_errors": errors, "controlled_inputs": expected if matched else None,
            "groups": groups, "summary": summaries, "sample_sufficient": sufficient,
            "observed_median_reduction_seconds": reduction,
            "observed_median_reduction_percent": 100 * reduction / before if reduction is not None else None,
            "basis": "Original acceptance timing only, including evidence export/validation and excluding cleanup. FAIL/unverified runs remain listed and never enter successful timing statistics. Matching inputs do not prove equal environment, provider load, or semantic quality beyond the named validator. Source revisions may differ between groups, never within one group.",
            "sample_note": "Each group has at least three timed original passes; medians/ranges describe this small sample, not project p95 or proven causality." if sufficient else "样本不足：每组至少需要 3 次有原始验收通过计时的独立运行；当前差异仅为观测，不证明稳定提速。"}


def render_comparison(report):
    lines = ["# PwnMesh 相同样本耗时对照", "",
             "输入匹配：" + ("通过" if report["matched"] else "拒绝比较；不计算聚合耗时和提升比例"), "",
             report["sample_note"], "", "| 组 | 次序 | 原始验收 | 验收秒 | 计时状态 |",
             "| --- | ---: | --- | ---: | --- |"]
    for label, rows in report["groups"].items():
        for index, row in enumerate(rows, 1):
            value = row["time_to_acceptance_seconds"]
            lines.append(f"| {label} | {index} | {row['status']} | {value:.3f} | {row['timing_status']} |" if value is not None else f"| {label} | {index} | {row['status']} | — | {row['timing_status']} |")
    lines += ["", "| 组 | 尝试 / 原始通过 / 有效计时 | 中位秒 | 范围秒 |", "| --- | ---: | ---: | --- |"]
    for label, summary in report["summary"].items():
        counts = f"{summary['attempts']} / {summary['original_passes']} / {summary['timed_passes']}"
        if summary["median_seconds"] is None:
            lines.append(f"| {label} | {counts} | — | — |")
        else:
            lines.append(f"| {label} | {counts} | {summary['median_seconds']:.3f} | {summary['min_seconds']:.3f}–{summary['max_seconds']:.3f} |")
    if report["observed_median_reduction_percent"] is not None:
        lines += ["", f"观测中位耗时减少 {report['observed_median_reduction_seconds']:.3f} 秒（{report['observed_median_reduction_percent']:.2f}%；负值表示变慢）。"]
    if report["matching_errors"]:
        lines += ["", *["- " + error for error in report["matching_errors"]]]
    lines += ["", report["basis"], "", "各次目录、源码身份及完整匹配字段见 comparison.json。", ""]
    return "\n".join(lines)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--baseline", action="append", type=Path, help="baseline run directory; repeat for each attempt")
    parser.add_argument("--candidate", action="append", type=Path, help="candidate run directory; repeat for each attempt")
    args = parser.parse_args()
    if args.baseline or args.candidate:
        if not args.baseline or not args.candidate:
            parser.error("--baseline and --candidate are both required for comparison")
        report = compare_runs(args.baseline, args.candidate)
        args.output.mkdir(parents=True, exist_ok=True)
        (args.output / "comparison.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        (args.output / "comparison.md").write_text(render_comparison(report), encoding="utf-8")
        print(json.dumps({"report": str(args.output / "comparison.md"), "matched": report["matched"], "sample_sufficient": report["sample_sufficient"]}))
        if not report["matched"]:
            raise SystemExit(2)
        return
    report = analyze(args.output)
    (args.output / "efficiency-report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    (args.output / "efficiency-report.md").write_text(render(report), encoding="utf-8")
    print(json.dumps({"report": str(args.output / "efficiency-report.md"), "project_wall_seconds": report["project_wall_seconds"], "model_calls": report["model_calls"]}))


if __name__ == "__main__":
    main()
