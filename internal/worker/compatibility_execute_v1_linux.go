//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"

	"pwnmesh/internal/workergraph"
)

// Only existing execute-v1 checkpoints use the old scheduler. Keeping its
// original definition preserves hashes and uncertain-callback recovery without
// reconstructing or weakening the legacy checkpoint protocol. A successful
// migration is sealed by acceptance.json schema 1; new runs never create this
// DAG. The execute-v1 literal and node definitions are compatibility identities.
// See docs/compatibility.md before changing or removing this adapter.
func resumeExecuteV1Acceptance(ctx context.Context, runID string, input json.RawMessage, dir string, call func(context.Context) (acceptanceOutput, error), verify func(context.Context, acceptanceOutput) error) (acceptanceOutput, error) {
	prepare := func(ctx context.Context, _ workergraph.Input) (workergraph.Output, error) {
		return workergraph.Output{}, verify(ctx, acceptanceOutput{})
	}
	run := func(ctx context.Context, _ workergraph.Input) (workergraph.Output, error) {
		out, err := call(ctx)
		if errors.Is(err, ErrInterrupted) {
			err = errors.Join(workergraph.ErrInterrupted, err)
		}
		return executeV1AcceptanceOutput(out), err
	}
	check := func(ctx context.Context, _ workergraph.Input, out workergraph.Output) error {
		return verify(ctx, fromExecuteV1Acceptance(out))
	}
	accept := func(_ context.Context, in workergraph.Input) (workergraph.Output, error) {
		return in.Dependencies[0].Output, nil
	}
	definition := workergraph.Definition{Version: "execute-v1", Nodes: []workergraph.Node{
		{ID: "prepare", Kind: "function", Run: prepare, Verify: check, Reconcile: func(ctx context.Context, in workergraph.Input, _ workergraph.NodeState) (workergraph.Output, error) {
			return prepare(ctx, in)
		}},
		{ID: "agent", Kind: "agent", DependsOn: []workergraph.Dependency{{ID: "prepare"}}, Run: run, Verify: check, Reconcile: func(ctx context.Context, in workergraph.Input, _ workergraph.NodeState) (workergraph.Output, error) {
			return run(ctx, in)
		}},
		{ID: "accept", Kind: "function", DependsOn: []workergraph.Dependency{{ID: "agent"}}, Run: accept, Verify: check, Reconcile: func(ctx context.Context, in workergraph.Input, _ workergraph.NodeState) (workergraph.Output, error) {
			return accept(ctx, in)
		}},
	}}
	checkpoint, err := workergraph.Run(ctx, definition, workergraph.Options{RunID: runID, Input: input, Dir: dir, Parallelism: 1})
	if errors.Is(err, workergraph.ErrInterrupted) {
		err = errors.Join(ErrInterrupted, err)
	}
	if err != nil {
		return acceptanceOutput{}, err
	}
	for _, node := range checkpoint.Nodes {
		if node.ID == "accept" && node.Status == "succeeded" && checkpoint.Status == "succeeded" {
			return fromExecuteV1Acceptance(node.Output), nil
		}
	}
	return acceptanceOutput{}, errors.New("legacy graph has no durable accepted result")
}

func executeV1AcceptanceOutput(out acceptanceOutput) workergraph.Output {
	converted := workergraph.Output{Value: out.Value}
	for _, artifact := range out.Artifacts {
		converted.Artifacts = append(converted.Artifacts, workergraph.Artifact(artifact))
	}
	return converted
}

func fromExecuteV1Acceptance(out workergraph.Output) acceptanceOutput {
	converted := acceptanceOutput{Value: out.Value}
	for _, artifact := range out.Artifacts {
		converted.Artifacts = append(converted.Artifacts, acceptanceArtifact(artifact))
	}
	return converted
}
