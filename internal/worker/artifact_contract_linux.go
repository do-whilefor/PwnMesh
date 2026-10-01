//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"pwnmesh/internal/workergraph"
)

type commandGraphInput struct {
	Node     string `json:"node"`
	Artifact string `json:"artifact"`
}

// Keep durable outputs unchanged: their exact JSON participates in checkpoint
// input hashes. Views add named files only at model and dependency-file edges.
type commandGraphOutputView struct {
	workergraph.Output
	Files map[string]workergraph.Artifact `json:"files"`
}

type commandGraphNodeView struct {
	workergraph.NodeState
	Output commandGraphOutputView `json:"output"`
}

func commandGraphViews(states []workergraph.NodeState) []commandGraphNodeView {
	views := make([]commandGraphNodeView, 0, len(states))
	for _, state := range states {
		views = append(views, commandGraphNodeView{state, commandGraphOutputView{state.Output, commandOutputFiles(state.Output)}})
	}
	return views
}

func commandOutputFiles(output workergraph.Output) map[string]workergraph.Artifact {
	files := make(map[string]workergraph.Artifact)
	var value commandGraphOutput
	if json.Unmarshal(output.Value, &value) != nil || !filepath.IsAbs(value.OutputPath) || filepath.Base(value.OutputPath) != "stdout.log" || filepath.Clean(value.OutputPath) != value.OutputPath {
		return files
	}
	dir := filepath.Dir(value.OutputPath)
	for _, artifact := range output.Artifacts {
		name, err := filepath.Rel(dir, artifact.Path)
		if err == nil && commandArtifactPath(name) && filepath.Join(dir, name) == artifact.Path {
			files[name] = artifact
		}
	}
	return files
}

// A declaration is required only when a consumer explicitly requests that
// file. Historical log-only nodes and graphs without inputs stay valid.
func validateCommandInputs(spec *commandGraphSpec) error {
	byID := make(map[string]commandGraphNode, len(spec.Nodes))
	for _, node := range spec.Nodes {
		byID[node.ID] = node
	}
	for i := range spec.Nodes {
		node := &spec.Nodes[i]
		if len(node.Inputs) > 64 {
			return fmt.Errorf("node %s supports at most 64 artifact inputs", node.ID)
		}
		slices.SortFunc(node.Inputs, func(a, b commandGraphInput) int {
			if order := strings.Compare(a.Node, b.Node); order != 0 {
				return order
			}
			return strings.Compare(a.Artifact, b.Artifact)
		})
		for k, input := range node.Inputs {
			if !commandGraphID.MatchString(input.Node) || !commandArtifactPath(input.Artifact) {
				return fmt.Errorf("node %s input requires a safe producer node and declared artifact path", node.ID)
			}
			if k > 0 && input == node.Inputs[k-1] {
				return fmt.Errorf("node %s has duplicate artifact input %s/%s", node.ID, input.Node, input.Artifact)
			}
			if !slices.ContainsFunc(node.DependsOn, func(dep workergraph.Dependency) bool { return dep.ID == input.Node && !dep.Optional }) {
				return fmt.Errorf("node %s input %s/%s requires a direct non-optional dependency", node.ID, input.Node, input.Artifact)
			}
			producer, exists := byID[input.Node]
			if !exists || !slices.Contains(producer.Artifacts, input.Artifact) {
				return fmt.Errorf("node %s input %s/%s is undeclared; add %q to producer %s artifacts", node.ID, input.Node, input.Artifact, input.Artifact, input.Node)
			}
		}
	}
	return nil
}

// Freeze only declared inputs before starting a consumer. Hash the same bytes
// that are copied, then expose their private paths through output.files. Raw
// producer receipts remain unchanged for provenance and checkpoint binding.
func freezeCommandInputs(ctx context.Context, dir string, spec commandGraphNode, dependencies []workergraph.NodeState) ([]commandGraphNodeView, error) {
	views := commandGraphViews(dependencies)
	byID := make(map[string]int, len(views))
	for i := range views {
		byID[views[i].ID] = i
		views[i].Output.Files = map[string]workergraph.Artifact{}
	}
	if len(spec.Inputs) == 0 {
		return views, nil
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || !filepath.IsAbs(dir) || resolved != dir {
		return nil, errors.New("consumer input directory must be an absolute directory without symlinks")
	}
	root, err := os.MkdirTemp(dir, ".inputs-")
	if err != nil {
		return nil, err
	}
	complete := false
	defer func() {
		if !complete {
			os.RemoveAll(root)
		}
	}()
	for _, input := range spec.Inputs {
		if !commandGraphID.MatchString(input.Node) || !commandArtifactPath(input.Artifact) {
			return nil, fmt.Errorf("node %s input requires a safe producer node and declared artifact path", spec.ID)
		}
		index, exists := byID[input.Node]
		if !exists || views[index].Status != "succeeded" {
			return nil, fmt.Errorf("node %s input %s/%s requires a successful producer receipt", spec.ID, input.Node, input.Artifact)
		}
		dependency := &views[index]
		artifact, exists := commandOutputFiles(dependency.NodeState.Output)[input.Artifact]
		if !exists {
			return nil, fmt.Errorf("node %s input %s/%s is missing from its producer receipt", spec.ID, input.Node, input.Artifact)
		}
		var value commandGraphOutput
		if err := json.Unmarshal(dependency.Output.Value, &value); err != nil {
			return nil, err
		}
		f, err := os.CreateTemp(root, "input-")
		if err != nil {
			return nil, err
		}
		var writeErr error
		current, _, readErr := inspectCommandFileChunks(ctx, filepath.Dir(value.OutputPath), input.Artifact, 0, func(chunk []byte) {
			if writeErr == nil {
				_, writeErr = f.Write(chunk)
			}
		})
		if readErr == nil && current != artifact {
			readErr = errors.New("SHA-256 changed since its producer succeeded")
		}
		if err = errors.Join(readErr, writeErr, f.Chmod(0400), f.Sync(), f.Close()); err != nil {
			return nil, fmt.Errorf("node %s input %s/%s: %w", spec.ID, input.Node, input.Artifact, err)
		}
		dependency.Output.Files[input.Artifact] = workergraph.Artifact{Path: f.Name(), SHA256: artifact.SHA256}
	}
	complete = true
	return views, nil
}
