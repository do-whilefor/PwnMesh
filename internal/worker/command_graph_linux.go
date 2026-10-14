//go:build linux

package worker

import (
	"context"
	"encoding/json"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/worker/dag"
	"pwnmesh/internal/workergraph"
)

// Each call runs the currently known graph. The parent Loop can observe its
// results and append tasks on a later call; every child owns a separate Loop.
func commandGraphTool(j Job, o Options) agent.Tool {
	tool := agent.Tool{Definition: agent.Definition{
		Name: "run_graph",
		Description: "Run a command/agent DAG; success covers submitted nodes, not Step completion. After observing results, extend the same key with the full cumulative list and unchanged prior nodes. Completed nodes are verified, never replayed. " +
			"command runs bash; agent owns a separate Loop with task and file/bash tools. Children cannot delegate, publish or finish the Step. Supply their context and synthesize results; same-Run Agents are not independent reviewers. Nodes share a container, not security sandboxes. " +
			"PWNMESH_WORKSPACE and PWNMESH_NODE_DIR are absolute. Create declared artifacts relative to PWNMESH_NODE_DIR; use JSON artifacts for structured results. PWNMESH_DEPENDENCIES is the path to a JSON file containing an array of {id,status,output}. Consume named output.files[name].path; these are verified private read-only copies of declared inputs, selected from direct required dependencies. Missing declarations fail before execution; changed bytes fail before consumption. output.artifacts retains producer receipts. " +
			"output.value has stdout/stderr previews and output_path/stderr_path full logs; failed output is diagnostic data without accepted artifacts. Logs are separate from output.files. Agent output has only stdout and is an account, not raw evidence. verified_evidence contains verified command logs; preview_complete=true gives exact UTF-8 content and line bounds usable by finish_step. Assess meaning and inspect truncated, changed or additional evidence. observed_at is verification time, not acceptance. " +
			"Declare shared mutable resources and order nodes sharing them; resources:[] asserts independent effects. Required dependencies must succeed. when checks dependency status and optional full verified stdout text. Failed/skipped routes require optional edges; failed routes also require an optional source node. " +
			"Required failure stops new nodes while active independent nodes settle; that key is terminal. Repair under a new key, using reuse_from to import successful nodes from this parent Run without overriding their definitions or replaying effects. Changed/missing source definitions or files, unresolved operations and reuse chains fail. " +
			"Default parallelism=min(node count,16); command timeout<=120s, agent<=600s, files<=64 MiB, all within the parent deadline.",
		Schema: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$"},"parallelism":{"type":"integer","minimum":1,"maximum":16},"nodes":{"type":"array","minItems":1,"maxItems":64,"items":{"type":"object","properties":{"id":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$"},"kind":{"type":"string","enum":["command","agent","reuse"]},"command":{"type":"string","minLength":1,"maxLength":32768},"task":{"type":"string","minLength":1,"maxLength":32768},"timeout":{"type":"integer","minimum":1,"maximum":600},"depends_on":{"type":"array","maxItems":64,"items":{"type":"object","properties":{"id":{"type":"string"},"optional":{"type":"boolean"}},"required":["id"],"additionalProperties":false}},"resources":{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":128}},"artifacts":{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":256},"description":"Declared output files relative to PWNMESH_NODE_DIR; [] means logs only."},"optional":{"type":"boolean"},"when":{"type":"object","properties":{"node":{"type":"string"},"status":{"type":"string","enum":["succeeded","failed","skipped"]},"contains":{"type":"string","minLength":1,"maxLength":256}},"required":["node"],"additionalProperties":false},"inputs":{"type":"array","maxItems":64,"items":{"type":"object","properties":{"node":{"type":"string"},"artifact":{"type":"string"}},"required":["node","artifact"],"additionalProperties":false},"description":"Required named files from direct required dependencies; validated before graph execution and before consumption."},"reuse_from":{"type":"object","properties":{"key":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$"},"node":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$"}},"required":["key","node"],"additionalProperties":false}},"required":["id","resources","inputs"],"additionalProperties":false}}},"required":["key","nodes"],"additionalProperties":false}`),
	}, Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
		return runCommandGraph(ctx, j, o, raw)
	}}
	return tool
}

func commandGraphScope(j Job, runDir string) dag.Scope {
	return dag.Scope{RunID: j.RunID, RunDir: runDir, Workspace: j.Workspace, Identity: func(dir string) (json.RawMessage, error) {
		identity, err := identityFor(j, dir)
		if err != nil {
			return nil, err
		}
		return json.Marshal(identity)
	}}
}

func runCommandGraph(ctx context.Context, j Job, o Options, raw json.RawMessage) (string, error) {
	if !o.graphDeadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, o.graphDeadline)
		defer cancel()
	}
	return dag.Run(ctx, commandGraphScope(j, o.RunDir), raw, func(ctx context.Context, node dag.AgentNode) (workergraph.Output, error) {
		return executeAgentNode(ctx, j, o, node)
	})
}
