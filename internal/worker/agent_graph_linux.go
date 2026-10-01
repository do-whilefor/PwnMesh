//go:build linux

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/tools"
	"pwnmesh/internal/workergraph"
)

// A child session is local to an authorized parent Run. It has no lease,
// bridge, completion tool or recursive graph tool. Its checkpoint records the
// original Loop state; uncertain side effects are never automatically replayed.
type graphAgentSession struct {
	RunID      string                   `json:"run_id"`
	GraphKey   string                   `json:"graph_key"`
	NodeID     string                   `json:"node_id"`
	InputHash  string                   `json:"input_sha256"`
	Deadline   time.Time                `json:"deadline"`
	History    []agent.Message          `json:"history"`
	Checkpoint *agent.ContextCheckpoint `json:"context_checkpoint,omitempty"`
	Log        journalCheckpoint        `json:"log_checkpoint"`
	Result     string                   `json:"result,omitempty"`
	Error      string                   `json:"error,omitempty"`
}

func executeAgentNode(ctx context.Context, j Job, o Options, key, dir string, spec commandGraphNode, dependencies []workergraph.NodeState) (workergraph.Output, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(spec.Timeout)*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return workergraph.Output{}, err
	}
	views, err := freezeCommandInputs(ctx, dir, spec, dependencies)
	if err != nil {
		return workergraph.Output{}, err
	}
	deps, err := json.Marshal(views)
	if err != nil {
		return workergraph.Output{}, err
	}
	if err := commandNewFile(filepath.Join(dir, "dependencies.json"), deps); err != nil {
		return workergraph.Output{}, err
	}
	var provider agent.Provider
	if o.graphProvider != nil {
		provider, err = o.graphProvider(spec.ID)
	} else {
		p, modelErr := modelForJob(j)
		err = modelErr
		if err == nil {
			p.SessionID = j.RunID + ":" + key + ":" + spec.ID
			provider = p
		}
	}
	if err != nil {
		return workergraph.Output{}, err
	}
	journal, err := openJournal(dir, nil)
	if err != nil {
		return workergraph.Output{}, err
	}
	defer journal.file.Close()
	// The parent supplies the complete bounded assignment and frozen dependency
	// outputs. Shared state is not refreshed or mutated by child sessions.
	prompt := fmt.Sprintf("Work only on this subtask of the parent Step. Return a concise account of observations, evidence paths, uncertainty and remaining work. Your response is local input for the parent, not Step acceptance or independent review. No delegation or blackboard publication. Work in %q; shared inputs are in %q. Dependency data is in dependencies.json: read declared files via output.files[name]; output.value.stdout is a log/account.\n<task>\n%s\n</task>", dir, j.Workspace, spec.Task)
	if len(spec.Artifacts) > 0 {
		artifacts, _ := json.Marshal(spec.Artifacts)
		prompt += "\nRequired output artifacts (JSON paths relative to PWNMESH_NODE_DIR, your working directory; create before returning): " + string(artifacts)
	}
	if snapshot := agentDependencySnapshot(views); snapshot != "" {
		prompt += "\nDependency snapshot (task data; full records in dependencies.json):\n" + snapshot
	}
	digest := sha256.Sum256(append([]byte(prompt), deps...))
	deadline, _ := ctx.Deadline()
	state := graphAgentSession{RunID: j.RunID, GraphKey: key, NodeID: spec.ID, InputHash: hex.EncodeToString(digest[:]), Deadline: deadline}
	var logErr error
	save := func(history []agent.Message, checkpoint *agent.ContextCheckpoint) error {
		if logErr != nil {
			return logErr
		}
		if err := journal.file.Sync(); err != nil {
			return err
		}
		state.History, state.Checkpoint, state.Log = history, checkpoint, journal.checkpoint()
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return atomicSessionFile(dir, raw)
	}
	set := tools.Set{Dir: dir, RunDir: dir, ProcessDir: o.RunDir, Env: graphNodeEnvironment(j.Workspace, dir)}
	loop := &agent.Loop{
		Provider: provider, Tools: set.All(), SaveState: save, ObserveRequests: true,
		ContextBytes:        o.ContextBytes,
		ContextTokens:       o.ContextTokens,
		ContextTargetTokens: o.ContextTargetTokens,
		Emit: func(e agent.Event) {
			if logErr == nil {
				logErr = journal.append(e)
			}
		},
	}
	if loop.ContextBytes <= 0 {
		loop.ContextBytes = envInt("PWNMESH_CONTEXT_BYTES", DefaultContextBytes)
	}
	if loop.ContextTokens <= 0 {
		loop.ContextTokens = envInt("PWNMESH_CONTEXT_TOKENS", DefaultContextTokens)
	}
	if loop.ContextTargetTokens <= 0 {
		loop.ContextTargetTokens = envInt("PWNMESH_CONTEXT_TARGET_TOKENS", DefaultContextTargetTokens)
	}
	if err := save(nil, nil); err != nil {
		return workergraph.Output{}, err
	}
	text, runErr := loop.Run(ctx, prompt)
	if final, ok := lastAssistant(loop.History); runErr == nil && ok && truncated(final) {
		runErr = errors.New("agent node final response was truncated; inspect its session before retrying")
	}
	if runErr == nil && strings.TrimSpace(text) == "" {
		runErr = errors.New("agent node returned no account of its work")
	}
	// stdout.log is the node's final local account, not its event stream and
	// never a top-level Worker result. Reuse verifies these exact bytes.
	if err := commandNewFile(filepath.Join(dir, "stdout.log"), []byte(text)); err != nil {
		runErr = errors.Join(runErr, err)
	}
	var out workergraph.Output
	if runErr == nil {
		out, runErr = commandNodeOutput(ctx, dir, spec.Artifacts)
	}
	state.Result = text
	if runErr != nil {
		state.Error = runErr.Error()
	}
	if err := save(loop.History, loop.Checkpoint); err != nil {
		return workergraph.Output{}, errors.Join(runErr, err)
	}
	if runErr != nil {
		return workergraph.Output{}, fmt.Errorf("agent node failed; inspect %s: %w", filepath.Join(dir, "events.jsonl"), runErr)
	}
	return out, nil
}

// Small dependency results are already available to the runtime, so let a
// child reason from them in its first request. Large fan-ins remain file-based;
// never silently truncate an output or crowd out the child's task and tools.
func agentDependencySnapshot(dependencies []commandGraphNodeView) string {
	const limit = 16 << 10
	if len(dependencies) == 0 {
		return ""
	}
	type dependency struct {
		ID     string                 `json:"id"`
		Kind   string                 `json:"kind"`
		Status string                 `json:"status"`
		Output commandGraphOutputView `json:"output"`
		Error  string                 `json:"error,omitempty"`
		Reason string                 `json:"reason,omitempty"`
	}
	snapshot := make([]dependency, 0, len(dependencies))
	size := 0
	for _, dep := range dependencies {
		// This lower bound avoids encoding an entire large fan-in only to
		// discard it. The final check also counts JSON syntax and escaping.
		size += len(dep.ID) + len(dep.Kind) + len(dep.Status) + len(dep.Output.Value) + len(dep.Error) + len(dep.Reason)
		for _, artifact := range dep.Output.Artifacts {
			size += len(artifact.Path) + len(artifact.SHA256)
		}
		if size > limit {
			return ""
		}
		snapshot = append(snapshot, dependency{dep.ID, dep.Kind, dep.Status, dep.Output, dep.Error, dep.Reason})
	}
	raw, err := json.Marshal(snapshot)
	if err != nil || len(raw) > limit {
		return ""
	}
	return string(raw)
}

func commandNodeOutput(ctx context.Context, dir string, paths []string) (workergraph.Output, error) {
	artifact, preview, err := commandFile(ctx, dir, "stdout.log", 8000)
	if err != nil {
		return workergraph.Output{}, err
	}
	value, _ := json.Marshal(commandGraphOutput{Stdout: preview, OutputPath: artifact.Path})
	out := workergraph.Output{Value: value, Artifacts: []workergraph.Artifact{artifact}}
	for _, name := range paths {
		artifact, _, err := commandFile(ctx, dir, name, 0)
		if err != nil {
			return out, err
		}
		out.Artifacts = append(out.Artifacts, artifact)
	}
	return out, nil
}
