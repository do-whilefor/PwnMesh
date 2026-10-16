//go:build linux

package dag

import (
	"context"
	"encoding/json"
	"path/filepath"

	"pwnmesh/internal/workergraph"
)

// AgentNode carries one validated child assignment. The parent owns the Agent
// session; this package owns the dependency files and artifact receipts.
type AgentNode struct {
	Key          string
	Dir          string
	Spec         Node
	InputSHA256  string
	dependencies []workergraph.NodeState
	workspace    string
}

// AgentExecutor separates fresh execution from read-only reconciliation of a
// committed child result. Reconcile must never resume an unfinished Agent.
type AgentExecutor struct {
	Run       func(context.Context, AgentNode) (workergraph.Output, error)
	Reconcile func(context.Context, AgentNode) (workergraph.Output, error)
}

// PrepareInputs freezes declared dependency bytes and publishes their exact view.
// Call it before starting the child session or issuing a model request.
func (n AgentNode) PrepareInputs(ctx context.Context) ([]NodeView, json.RawMessage, error) {
	views, err := freezeCommandInputs(ctx, n.Dir, n.Spec, n.dependencies)
	if err != nil {
		return nil, nil, err
	}
	deps, err := json.Marshal(views)
	if err != nil {
		return nil, nil, err
	}
	if err := commandNewFile(filepath.Join(n.Dir, "dependencies.json"), deps); err != nil {
		return nil, nil, err
	}
	return views, deps, nil
}

// Environment binds file tools to this node without changing process ownership.
func (n AgentNode) Environment() []string { return graphNodeEnvironment(n.workspace, n.Dir) }

// RetainOutput stores the child's account before the parent saves terminal state.
// Failed Agents retain diagnostics but do not certify declared output artifacts.
func (n AgentNode) RetainOutput(ctx context.Context, text string, acceptArtifacts bool) (workergraph.Output, error) {
	if err := commandNewFile(filepath.Join(n.Dir, "stdout.log"), []byte(text)); err != nil {
		return workergraph.Output{}, err
	}
	var paths []string
	if acceptArtifacts {
		paths = n.Spec.Artifacts
	}
	return commandNodeOutput(ctx, n.Dir, paths)
}

func commandNodeOutput(ctx context.Context, dir string, paths []string) (workergraph.Output, error) {
	artifact, preview, err := commandFile(ctx, dir, "stdout.log", 8000)
	if err != nil {
		return workergraph.Output{}, err
	}
	value, _ := json.Marshal(Output{Stdout: preview, OutputPath: artifact.Path})
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
