//go:build linux

package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"pwnmesh/internal/workergraph"
)

// Reuse and shared-evidence handoff must interpret the same durable definition
// and input bindings. Reading a snapshot never replays work or changes a graph.
func readCommandSnapshot(ctx context.Context, j Job, runDir, key, dir string) (workergraph.Checkpoint, map[string]commandGraphNode, error) {
	var checkpoint workergraph.Checkpoint
	raw, err := readCommandGraphFile(ctx, filepath.Join(dir, "graph.json"), 8<<20)
	if err != nil {
		return checkpoint, nil, err
	}
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return checkpoint, nil, err
	}
	if len(checkpoint.Nodes) == 0 || len(checkpoint.Nodes) > 64 {
		return checkpoint, nil, errors.New("invalid command graph snapshot size")
	}
	spec := commandGraphSpec{Key: key}
	for _, node := range checkpoint.Nodes {
		digest, err := hex.DecodeString(node.DefinitionSHA256)
		if err != nil || len(digest) != sha256.Size {
			return checkpoint, nil, errors.New("command graph has no bound node definition")
		}
		raw, err := readCommandGraphFile(ctx, filepath.Join(dir, "definitions", node.DefinitionSHA256+".json"), 1<<20)
		if err != nil {
			return checkpoint, nil, fmt.Errorf("command graph requires retained source definitions: %w", err)
		}
		var nodeSpec commandGraphNode
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&nodeSpec); err != nil {
			return checkpoint, nil, err
		}
		canonical, _ := json.Marshal(nodeSpec)
		if !bytes.Equal(canonical, raw) || nodeSpec.ID != node.ID {
			return checkpoint, nil, errors.New("invalid command graph source definition")
		}
		spec.Nodes = append(spec.Nodes, nodeSpec)
	}
	if err := validateCommandGraph(&spec); err != nil {
		return checkpoint, nil, err
	}
	definition := workergraph.Definition{Version: commandGraphVersion}
	specs := make(map[string]commandGraphNode, len(spec.Nodes))
	states := make(map[string]workergraph.NodeState, len(checkpoint.Nodes))
	for _, state := range checkpoint.Nodes {
		states[state.ID] = state
	}
	for _, nodeSpec := range spec.Nodes {
		node := commandNodeDefinition(nodeSpec)
		if nodeSpec.Kind == "reuse" {
			// Imported nodes retain the original execution kind in their bound
			// definition. Their original artifact paths resolve to the source graph.
			node.Kind = states[node.ID].Kind
		}
		definition.Nodes = append(definition.Nodes, node)
		specs[nodeSpec.ID] = nodeSpec
	}
	identity, err := identityFor(j, runDir)
	if err != nil {
		return checkpoint, nil, err
	}
	input, err := commandGraphInitialInput(identity, key)
	if err != nil {
		return checkpoint, nil, err
	}
	if err := workergraph.ValidateSnapshot(definition, workergraph.Options{RunID: j.RunID, Input: input, Dir: dir, Parallelism: 1}, checkpoint); err != nil {
		return checkpoint, nil, err
	}
	return checkpoint, specs, nil
}

// A DAG artifact is an already accepted observation, not a mutable path that
// can silently be reinterpreted when the parent publishes a Fact. Bind the
// exact bytes being frozen to that receipt. Other local files keep the ordinary
// evidence-selection contract, including fresh observations made by the parent.
func verifyCommandEvidence(ctx context.Context, j Job, runDir, source string, raw []byte) error {
	runDir, err := filepath.Abs(runDir)
	if err != nil {
		return err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(filepath.Join(runDir, "graph-tools"), source)
	if err != nil {
		return err
	}
	parts := strings.SplitN(relative, string(filepath.Separator), 4)
	if len(parts) != 4 || !commandGraphID.MatchString(parts[0]) || parts[1] != "nodes" || !commandGraphID.MatchString(parts[2]) {
		return nil
	}
	dir, err := existingCommandGraphDirectory(runDir, "graph-tools", parts[0])
	if err != nil {
		return err
	}
	checkpoint, specs, err := readCommandSnapshot(ctx, j, runDir, parts[0], dir)
	if err != nil {
		return err
	}
	spec, exists := specs[parts[2]]
	if !exists || parts[3] != "stdout.log" && parts[3] != "stderr.log" && !slices.Contains(spec.Artifacts, parts[3]) {
		return nil
	}
	for _, node := range checkpoint.Nodes {
		if node.ID != spec.ID {
			continue
		}
		if node.Status != "succeeded" {
			if parts[3] == "stdout.log" || parts[3] == "stderr.log" {
				// Failed logs remain observable diagnostics. They carry no accepted
				// artifact receipt and cannot claim the successful-output shortcut.
				return nil
			}
			return errors.New("selected DAG artifact has not been accepted")
		}
		for _, artifact := range node.Output.Artifacts {
			if artifact.Path == source {
				digest := sha256.Sum256(raw)
				if hex.EncodeToString(digest[:]) != artifact.SHA256 {
					return errors.New("selected DAG artifact SHA-256 changed after node acceptance; inspect and retain a new observation")
				}
				return nil
			}
		}
	}
	return errors.New("selected DAG artifact has no accepted receipt")
}
