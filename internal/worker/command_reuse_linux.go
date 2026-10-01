//go:build linux

package worker

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
	"pwnmesh/internal/process"
	"pwnmesh/internal/workergraph"
)

type commandGraphReuse struct {
	Key  string `json:"key"`
	Node string `json:"node"`
}

type commandReuseReceipt struct {
	Key              string `json:"key"`
	Node             string `json:"node"`
	Kind             string `json:"kind"`
	DefinitionSHA256 string `json:"definition_sha256"`
	InputSHA256      string `json:"input_sha256"`
}

type commandReuse struct {
	SourceNode workergraph.NodeState
	SourceSpec commandGraphNode
	SourceDir  string
	receipt    commandReuseReceipt
}

// A definition-only descriptor keeps persisted definition hashes identical to
// actual execution. The caller replaces these inert callbacks before Run.
func commandNodeDefinition(spec commandGraphNode) workergraph.Node {
	input, _ := json.Marshal(spec)
	node := workergraph.Node{ID: spec.ID, Kind: "function", Input: input, DependsOn: spec.DependsOn, Optional: spec.Optional}
	if spec.Kind == "agent" {
		node.Kind = "agent"
	}
	node.Run = func(context.Context, workergraph.Input) (workergraph.Output, error) {
		return workergraph.Output{}, errors.New("definition-only node cannot execute")
	}
	if spec.When != nil {
		node.When = func(context.Context, workergraph.Input) (bool, string, error) {
			return false, "", errors.New("definition-only condition cannot execute")
		}
	}
	return node
}

// Each definition is content-addressed and published atomically without
// replacing an existing file. A crash during extension leaves old definitions
// intact; graph.json alone chooses the committed set.
func storeCommandDefinitions(dir string, spec commandGraphSpec, definition workergraph.Definition) error {
	definitionsDir, err := commandGraphDirectory(dir, "definitions")
	if err != nil {
		return err
	}
	byID := make(map[string]workergraph.Node, len(definition.Nodes))
	for _, node := range definition.Nodes {
		byID[node.ID] = node
	}
	for _, nodeSpec := range spec.Nodes {
		node, ok := byID[nodeSpec.ID]
		raw, err := json.Marshal(nodeSpec)
		if err != nil {
			return err
		}
		if !ok || !bytes.Equal(node.Input, raw) {
			return errors.New("command definition differs from submitted node")
		}
		path := filepath.Join(definitionsDir, workergraph.NodeDefinitionSHA256(node)+".json")
		if existing, err := readCommandGraphFile(context.Background(), path, 1<<20); err == nil {
			if !bytes.Equal(existing, raw) {
				return errors.New("command definition content changed")
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := writeCommandDefinition(path, raw); err != nil {
			return err
		}
	}
	return nil
}

func writeCommandDefinition(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".definition-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err == nil {
		err = os.Link(f.Name(), path)
	}
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// readCommandGraphFile rejects aliases, special files and unbounded retained
// JSON before decoding. It does not create or update a source graph file.
func readCommandGraphFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if resolved != path {
		return nil, errors.New("command graph source must not use symlinks")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > limit {
		return nil, errors.New("command graph source must be a bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || int64(len(raw)) != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return nil, errors.New("command graph source changed while reading")
	}
	return raw, ctx.Err()
}

func existingCommandGraphDirectory(root string, elements ...string) (string, error) {
	dir := root
	for _, element := range elements {
		dir = filepath.Join(dir, element)
		info, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("command reuse source must be a real directory")
		}
	}
	return dir, nil
}

func commandGraphInitialInput(identity executionIdentity, key string) (json.RawMessage, error) {
	return json.Marshal(struct {
		Identity executionIdentity `json:"identity"`
		Key      string            `json:"key"`
	}{identity, key})
}

// Preflight every import while holding all source graph locks. Callers retain
// cleanup until execution and final verification finish. No new graph callback
// may begin if any requested source fails this preflight.
func prepareCommandReuses(ctx context.Context, j Job, o Options, spec commandGraphSpec) (map[string]*commandReuse, func(), error) {
	result := map[string]*commandReuse{}
	var unlocks []func()
	cleanup := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
		unlocks = nil
	}
	fail := func(err error) (map[string]*commandReuse, func(), error) {
		cleanup()
		return nil, func() {}, err
	}
	keys := []string{}
	for _, node := range spec.Nodes {
		if node.Kind != "reuse" {
			continue
		}
		if node.ReuseFrom == nil || !commandGraphID.MatchString(node.ReuseFrom.Key) || !commandGraphID.MatchString(node.ReuseFrom.Node) || node.ReuseFrom.Key == spec.Key {
			return fail(errors.New("reuse requires a different safe graph key and source node"))
		}
		keys = append(keys, node.ReuseFrom.Key)
	}
	if len(keys) == 0 {
		return result, cleanup, nil
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	identity, err := identityFor(j, o.RunDir)
	if err != nil {
		return fail(err)
	}
	type sourceGraph struct {
		dir   string
		nodes map[string]workergraph.NodeState
		specs map[string]commandGraphNode
	}
	sources := map[string]sourceGraph{}
	for _, key := range keys {
		dir, err := existingCommandGraphDirectory(o.RunDir, "graph-tools", key)
		if err != nil {
			return fail(err)
		}
		lockInfo, err := os.Lstat(filepath.Join(dir, "worker.lock"))
		if err != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode()&os.ModeSymlink != 0 {
			return fail(errors.New("reuse source lock must be an existing regular file"))
		}
		unlock, err := process.Lock(dir)
		if err != nil {
			return fail(err)
		}
		unlocks = append(unlocks, unlock)
		raw, err := readCommandGraphFile(ctx, filepath.Join(dir, "graph.json"), 8<<20)
		if err != nil {
			return fail(err)
		}
		var checkpoint workergraph.Checkpoint
		if err := json.Unmarshal(raw, &checkpoint); err != nil {
			return fail(err)
		}
		if !slices.Contains([]string{"failed", "succeeded"}, checkpoint.Status) || len(checkpoint.Nodes) == 0 || len(checkpoint.Nodes) > 64 {
			return fail(errors.New("reuse source graph must be terminal"))
		}
		sourceSpec := commandGraphSpec{Key: key}
		for _, node := range checkpoint.Nodes {
			if node.Status == "running" || node.Status == "pending" {
				return fail(errors.New("reuse source has unresolved operations"))
			}
			digest, err := hex.DecodeString(node.DefinitionSHA256)
			if err != nil || len(digest) != 32 {
				return fail(errors.New("reuse source has no bound node definition"))
			}
			raw, err := readCommandGraphFile(ctx, filepath.Join(dir, "definitions", node.DefinitionSHA256+".json"), 1<<20)
			if err != nil {
				return fail(fmt.Errorf("reuse requires retained source definitions: %w", err))
			}
			var nodeSpec commandGraphNode
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&nodeSpec); err != nil {
				return fail(err)
			}
			canonical, _ := json.Marshal(nodeSpec)
			if !bytes.Equal(canonical, raw) || nodeSpec.ID != node.ID || nodeSpec.Kind == "reuse" || nodeSpec.ReuseFrom != nil {
				return fail(errors.New("reuse source definition is invalid or recursive"))
			}
			sourceSpec.Nodes = append(sourceSpec.Nodes, nodeSpec)
		}
		if err := validateCommandGraph(&sourceSpec); err != nil {
			return fail(err)
		}
		definition := workergraph.Definition{Version: "mixed-dag-v2"}
		source := sourceGraph{dir: dir, nodes: map[string]workergraph.NodeState{}, specs: map[string]commandGraphNode{}}
		for _, nodeSpec := range sourceSpec.Nodes {
			definition.Nodes = append(definition.Nodes, commandNodeDefinition(nodeSpec))
			source.specs[nodeSpec.ID] = nodeSpec
		}
		input, err := commandGraphInitialInput(identity, key)
		if err != nil {
			return fail(err)
		}
		if err := workergraph.ValidateSnapshot(definition, workergraph.Options{RunID: j.RunID, Input: input, Dir: dir, Parallelism: 1}, checkpoint); err != nil {
			return fail(err)
		}
		for _, node := range checkpoint.Nodes {
			source.nodes[node.ID] = node
			if node.Status == "succeeded" {
				if err := verifyCommandNodeOutput(ctx, filepath.Join(dir, "nodes", node.ID), source.specs[node.ID], node.Output); err != nil {
					return fail(fmt.Errorf("reuse source node %s verification: %w", node.ID, err))
				}
			}
		}
		sources[key] = source
	}
	for _, node := range spec.Nodes {
		if node.Kind != "reuse" {
			continue
		}
		source := sources[node.ReuseFrom.Key]
		state, ok := source.nodes[node.ReuseFrom.Node]
		if !ok || state.Status != "succeeded" {
			return fail(errors.New("reuse source node must have succeeded"))
		}
		result[node.ID] = &commandReuse{SourceNode: state, SourceSpec: source.specs[state.ID], SourceDir: filepath.Join(source.dir, "nodes", state.ID), receipt: commandReuseReceipt{Key: node.ReuseFrom.Key, Node: state.ID, Kind: state.Kind, DefinitionSHA256: state.DefinitionSHA256, InputSHA256: state.InputSHA256}}
	}
	return result, cleanup, nil
}

func (r *commandReuse) Output() workergraph.Output {
	var value commandGraphOutput
	_ = json.Unmarshal(r.SourceNode.Output.Value, &value)
	receipt := r.receipt
	value.ReusedFrom = &receipt
	raw, _ := json.Marshal(value)
	return workergraph.Output{Value: raw, Artifacts: slices.Clone(r.SourceNode.Output.Artifacts)}
}

func (r *commandReuse) Verify(ctx context.Context, out workergraph.Output) error {
	expected := r.Output()
	if !bytes.Equal(expected.Value, out.Value) || !slices.Equal(expected.Artifacts, out.Artifacts) {
		return errors.New("reused node output or provenance changed")
	}
	return verifyCommandNodeOutput(ctx, r.SourceDir, r.SourceSpec, r.SourceNode.Output)
}
