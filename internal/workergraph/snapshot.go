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
	return hash(describe(canonicalNode(node)))
}

func validSavedNode(state NodeState, node Node) bool {
	return state.Kind == node.Kind && state.Attempt >= 0 && state.Attempt <= 1 &&
		slices.Contains([]string{"pending", "running", "succeeded", "skipped", "failed", "blocked", "cancelled"}, state.Status) &&
		validateOutput(state.Output) == nil
}

// Validate the entire retained graph before any recovery callback or write.
// Nodes absent from the saved checkpoint are extension candidates; every saved
// node must retain its original definition and a closed dependency set.
func validateSavedNodes(nodes []Node, initial json.RawMessage, saved []NodeState, signature string) error {
	byID := make(map[string]Node, len(nodes))
	states := make(map[string]*NodeState, len(saved))
	for _, node := range nodes {
		byID[node.ID] = node
	}
	var definitions []nodeDefinition
	for i := range saved {
		state := &saved[i]
		node, ok := byID[state.ID]
		if !ok || i > 0 && saved[i-1].ID >= state.ID || !validSavedNode(*state, node) {
			return errors.New("invalid graph node checkpoint")
		}
		if state.DefinitionSHA256 != NodeDefinitionSHA256(node) {
			return fmt.Errorf("node %s definition changed", node.ID)
		}
		if !validSavedAttempt(*state) || state.Status == "skipped" && state.Reason == "" {
			return errors.New("invalid graph node attempt, timing or skip reason")
		}
		definitions = append(definitions, describe(node))
		states[state.ID] = state
	}
	if hash(definitions) != signature {
		return errors.New("graph checkpoint definition mismatch")
	}
	for _, node := range nodes {
		state := states[node.ID]
		if state == nil {
			continue
		}
		for _, dep := range node.DependsOn {
			if states[dep.ID] == nil {
				return fmt.Errorf("node %s saved dependency is missing", node.ID)
			}
		}
		if state.InputSHA256 != "" || state.Attempt > 0 || state.Status == "skipped" {
			if state.InputSHA256 != nodeInput(node, *state, initial, states).SHA256 {
				return fmt.Errorf("node %s input binding changed", node.ID)
			}
		}
		if state.Status != "succeeded" {
			continue
		}
		for _, dep := range node.DependsOn {
			status := states[dep.ID].Status
			if status == "pending" || status == "running" || !dep.Optional && status != "succeeded" {
				return fmt.Errorf("node %s saved dependency is not satisfied", node.ID)
			}
		}
	}
	return nil
}

func validSavedAttempt(state NodeState) bool {
	if (state.Status == "running" || state.Status == "succeeded") && (state.Attempt != 1 || state.StartedAt.IsZero()) {
		return false
	}
	if state.Status == "succeeded" && state.FinishedAt.IsZero() {
		return false
	}
	return (state.Status != "pending" && state.Status != "skipped" && state.Status != "blocked") || state.Attempt == 0
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
	return validateSavedNodes(nodes, options.Input, saved.Nodes, saved.DefinitionSHA256)
}
