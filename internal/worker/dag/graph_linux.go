//go:build linux

package dag

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"pwnmesh/internal/process"
	"pwnmesh/internal/workergraph"
)

// Run binds command and Agent nodes to the generic checkpoint scheduler.
func Run(ctx context.Context, scope Scope, raw json.RawMessage, runAgent func(context.Context, AgentNode) (workergraph.Output, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var spec Spec
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return "", err
	}
	if err := validateCommandGraph(&spec); err != nil {
		return "", err
	}
	identity, err := scope.identity(scope.RunDir)
	if err != nil {
		return "", err
	}
	reuses, releaseSources, err := prepareCommandReuses(ctx, scope, spec)
	if err != nil {
		return "", err
	}
	defer releaseSources()
	inputSpec := spec
	inputSpec.Nodes = slices.Clone(spec.Nodes)
	for i := range inputSpec.Nodes {
		if reused := reuses[inputSpec.Nodes[i].ID]; reused != nil {
			inputSpec.Nodes[i].Artifacts = reused.SourceSpec.Artifacts
		}
	}
	if err := validateCommandInputs(&inputSpec); err != nil {
		return "", err
	}
	dir, err := commandGraphDirectory(scope.RunDir, "graph-tools", spec.Key)
	if err != nil {
		return "", err
	}
	unlock, err := process.Lock(dir)
	if err != nil {
		return "", err
	}
	defer unlock()
	input, err := commandGraphInitialInput(identity, spec.Key)
	if err != nil {
		return "", err
	}
	definition := workergraph.Definition{Version: commandGraphVersion}
	for _, specNode := range spec.Nodes {
		nodeSpec := specNode
		nodeDir := filepath.Join(dir, "nodes", nodeSpec.ID)
		node := commandNodeDefinition(nodeSpec)
		if reused := reuses[nodeSpec.ID]; reused != nil {
			node.Kind = reused.SourceNode.Kind
			node.Run = func(ctx context.Context, _ workergraph.Input) (workergraph.Output, error) {
				if err := ctx.Err(); err != nil {
					return workergraph.Output{}, err
				}
				return reused.Output(), nil
			}
			node.Verify = func(ctx context.Context, _ workergraph.Input, out workergraph.Output) error {
				return reused.Verify(ctx, out)
			}
			definition.Nodes = append(definition.Nodes, node)
			continue
		}
		node.Run = func(ctx context.Context, in workergraph.Input) (workergraph.Output, error) {
			if err := process.CheckLaunch(scope.RunDir); err != nil {
				return workergraph.Output{}, err
			}
			if process.Cancelled(scope.RunDir) {
				return workergraph.Output{}, context.Canceled
			}
			if _, err := commandGraphDirectory(dir, "nodes", nodeSpec.ID); err != nil {
				return workergraph.Output{}, err
			}
			if nodeSpec.Kind == "agent" {
				if runAgent == nil {
					return workergraph.Output{}, errors.New("agent node requires an executor")
				}
				return runAgent(ctx, AgentNode{Key: spec.Key, Dir: nodeDir, Spec: nodeSpec, dependencies: in.Dependencies, workspace: scope.Workspace})
			}
			return executeCommandNode(ctx, scope.RunDir, scope.Workspace, nodeDir, nodeSpec, in.Dependencies)
		}
		node.Verify = func(ctx context.Context, _ workergraph.Input, out workergraph.Output) error {
			return verifyCommandNodeOutput(ctx, nodeDir, nodeSpec, out)
		}
		if nodeSpec.When != nil {
			node.When = func(ctx context.Context, in workergraph.Input) (bool, string, error) {
				if err := ctx.Err(); err != nil {
					return false, "", err
				}
				for _, dep := range in.Dependencies {
					if dep.ID == nodeSpec.When.Node {
						matches := dep.Status == nodeSpec.When.Status
						if matches && nodeSpec.When.Contains != "" {
							var err error
							depDir := filepath.Join(dir, "nodes", dep.ID)
							if reused := reuses[dep.ID]; reused != nil {
								depDir = reused.SourceDir
							}
							matches, err = commandOutputContains(ctx, depDir, dep.Output, nodeSpec.When.Contains)
							if err != nil {
								return false, "", err
							}
						}
						if matches {
							return true, "", nil
						}
						return false, "dependency status or output did not match condition", nil
					}
				}
				return false, "", errors.New("condition dependency missing")
			}
		}
		// No Reconcile callback: a crash after shell side effects but before the
		// durable receipt requires inspection, never an automatic command replay.
		definition.Nodes = append(definition.Nodes, node)
	}
	if err := storeCommandDefinitions(dir, spec, definition); err != nil {
		return "", err
	}
	started := time.Now()
	checkpoint, runErr := workergraph.Run(ctx, definition, workergraph.Options{RunID: scope.RunID, Input: input, Dir: dir, Parallelism: spec.Parallelism, Extend: true, DrainOnFailure: true})
	response := struct {
		Key            string            `json:"key"`
		Status         string            `json:"status"`
		ElapsedMS      float64           `json:"elapsed_ms"`
		Nodes          []NodeView        `json:"nodes"`
		ObservedAt     string            `json:"observed_at"`
		Verification   string            `json:"verification"`
		Evidence       []Evidence        `json:"verified_evidence"`
		EvidenceErrors map[string]string `json:"evidence_errors,omitempty"`
		Error          string            `json:"error,omitempty"`
	}{Key: spec.Key, Status: checkpoint.Status, ElapsedMS: float64(time.Since(started)) / float64(time.Millisecond), Nodes: commandGraphViews(checkpoint.Nodes),
		ObservedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		Verification: "Only verified_evidence entries passed final regular-file, no-symlink, full-file SHA-256 and stdout-preview checks after successful node execution. Node statuses describe execution history; evidence_errors identify outputs that changed or became unreadable before handoff. This verifies retained bytes, not task meaning or Step acceptance.",
		Evidence:     []Evidence{}, EvidenceErrors: map[string]string{}}
	for _, node := range checkpoint.Nodes {
		if runErr != nil {
			break // Recovery errors may leave old succeeded statuses unverified.
		}
		// An Agent's final account is an interpretation, not a raw observation.
		// Keep it in node output for the parent to assess, but never offer it as
		// the direct evidence shortcut accepted by finish_step.
		if node.Kind != "function" || node.Status != "succeeded" || len(node.Output.Artifacts) == 0 {
			continue
		}
		artifact := node.Output.Artifacts[0]
		var value Output
		if json.Unmarshal(node.Output.Value, &value) != nil {
			continue
		}
		outputDir := filepath.Join(dir, "nodes", node.ID)
		if reused := reuses[node.ID]; reused != nil {
			outputDir = reused.SourceDir
		}
		current, preview, err := inspectCommandFile(ctx, outputDir, "stdout.log", 8000)
		if err != nil {
			response.EvidenceErrors[node.ID] = err.Error()
			continue
		}
		if current != artifact || preview.Text != value.Stdout {
			response.EvidenceErrors[node.ID] = "stdout changed after node verification"
			continue
		}
		ref := Evidence{NodeID: node.ID, Path: artifact.Path, SHA256: artifact.SHA256}
		// Completeness and hashing come from the same streaming read. A large
		// unchanged file remains verified, but needs an explicit excerpt.
		if preview.Complete {
			ref.PreviewComplete = true
			ref.StartLine = 1
			ref.EndLine = strings.Count(preview.Text, "\n")
			if !strings.HasSuffix(preview.Text, "\n") {
				ref.EndLine++
			}
		}
		response.Evidence = append(response.Evidence, ref)
	}
	if runErr != nil {
		response.Error = runErr.Error()
	}
	encoded, err := json.Marshal(response)
	return string(encoded), errors.Join(runErr, err)
}
