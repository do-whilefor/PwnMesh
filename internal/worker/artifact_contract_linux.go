//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"fmt"
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

// Recheck requested bytes immediately before launching a consumer. A durable
// success receipt alone does not prove another node left its files unchanged.
func validateCommandInputFiles(ctx context.Context, spec commandGraphNode, dependencies []workergraph.NodeState) error {
	byID := make(map[string]workergraph.NodeState, len(dependencies))
	for _, dependency := range dependencies {
		byID[dependency.ID] = dependency
	}
	for _, input := range spec.Inputs {
		dependency, exists := byID[input.Node]
		if !exists || dependency.Status != "succeeded" {
			return fmt.Errorf("node %s input %s/%s requires a successful producer receipt", spec.ID, input.Node, input.Artifact)
		}
		artifact, exists := commandOutputFiles(dependency.Output)[input.Artifact]
		if !exists {
			return fmt.Errorf("node %s input %s/%s is missing from its producer receipt", spec.ID, input.Node, input.Artifact)
		}
		var value commandGraphOutput
		if err := json.Unmarshal(dependency.Output.Value, &value); err != nil {
			return err
		}
		current, _, err := commandFile(ctx, filepath.Dir(value.OutputPath), input.Artifact, 0)
		if err != nil {
			return fmt.Errorf("node %s input %s/%s: %w", spec.ID, input.Node, input.Artifact, err)
		}
		if current != artifact {
			return fmt.Errorf("node %s input %s/%s SHA-256 changed since its producer succeeded", spec.ID, input.Node, input.Artifact)
		}
	}
	return nil
}
