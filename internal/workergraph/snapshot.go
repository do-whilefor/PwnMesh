//go:build linux

package workergraph

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// NodeDefinitionSHA256 binds one canonical node definition, using the same
// dependency order as Run. It never invokes any callback.
func NodeDefinitionSHA256(node Node) string {
	// Match prepare's nil/empty normalization as well as dependency ordering.
	node.Input = append(json.RawMessage(nil), node.Input...)
	node.DependsOn = append([]Dependency(nil), node.DependsOn...)
	slices.SortFunc(node.DependsOn, func(a, b Dependency) int { return compareID(a.ID, b.ID) })
	return hash(describe(node))
}

// ValidateSnapshot checks a complete saved graph against its original definition
// and initial input without writing a checkpoint or invoking any callback. The
// caller must separately verify external artifacts before consuming outputs.
// Legacy checkpoints without node definition hashes are deliberately rejected.
func ValidateSnapshot(definition Definition, options Options, saved Checkpoint) error {
	nodes, signature, err := prepare(definition, options)
	if err != nil {
		return err
	}
	if saved.SchemaVersion != 1 || saved.RunID != options.RunID || saved.Version != definition.Version || saved.DefinitionSHA256 != signature || saved.InputSHA256 != hash(options.Input) || len(saved.Nodes) != len(nodes) || !slices.Contains([]string{"running", "succeeded", "failed", "interrupted"}, saved.Status) {
		return errors.New("graph snapshot identity, definition or input mismatch")
	}
	byID := make(map[string]Node, len(nodes))
	states := make(map[string]*NodeState, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = node
	}
	for i := range saved.Nodes {
		state := &saved.Nodes[i]
		node, ok := byID[state.ID]
		if !ok || i > 0 && saved.Nodes[i-1].ID >= state.ID || state.Kind != node.Kind || state.DefinitionSHA256 != NodeDefinitionSHA256(node) || state.Attempt < 0 || state.Attempt > 1 || !slices.Contains([]string{"pending", "running", "succeeded", "skipped", "failed", "blocked", "cancelled"}, state.Status) || validateOutput(state.Output) != nil {
			return errors.New("invalid graph snapshot node")
		}
		if (state.Status == "running" || state.Status == "succeeded") && (state.Attempt != 1 || state.StartedAt.IsZero()) || state.Status == "succeeded" && state.FinishedAt.IsZero() || (state.Status == "pending" || state.Status == "skipped" || state.Status == "blocked") && state.Attempt != 0 || state.Status == "skipped" && state.Reason == "" {
			return errors.New("invalid graph snapshot attempt or timing")
		}
		states[state.ID] = state
	}
	for _, node := range nodes {
		state := states[node.ID]
		if state.InputSHA256 != "" || state.Attempt > 0 || state.Status == "skipped" {
			if state.InputSHA256 != inputHash(nodeInput(node, *state, options.Input, states)) {
				return fmt.Errorf("node %s snapshot input binding changed", node.ID)
			}
		}
		if state.Status != "succeeded" {
			continue
		}
		for _, dep := range node.DependsOn {
			status := states[dep.ID].Status
			if status == "pending" || status == "running" || !dep.Optional && status != "succeeded" {
				return fmt.Errorf("node %s snapshot dependency is not satisfied", node.ID)
			}
		}
	}
	return nil
}
