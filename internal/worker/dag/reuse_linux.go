//go:build linux

package dag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/sys/unix"
	"pwnmesh/internal/process"
	"pwnmesh/internal/workergraph"
)

type Reuse struct {
	Key  string `json:"key"`
	Node string `json:"node"`
}

type ReuseReceipt struct {
	Key                 string `json:"key"`
	Node                string `json:"node"`
	Kind                string `json:"kind"`
	DefinitionSHA256    string `json:"definition_sha256"`
	InputSHA256         string `json:"input_sha256"`
	ExecutionStartedAt  string `json:"execution_started_at,omitempty"`
	ExecutionFinishedAt string `json:"execution_finished_at,omitempty"`
}

type commandReuse struct {
	SourceNode workergraph.NodeState
	SourceSpec Node
	SourceDir  string
	receipt    ReuseReceipt
}

// A definition-only descriptor keeps persisted definition hashes identical to
// actual execution. The caller replaces these inert callbacks before Run.
func commandNodeDefinition(spec Node) workergraph.Node {
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
func storeCommandDefinitions(dir string, spec Spec, definition workergraph.Definition) error {
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

func commandGraphInitialInput(identity json.RawMessage, key string) (json.RawMessage, error) {
	return json.Marshal(struct {
		Identity json.RawMessage `json:"identity"`
		Key      string          `json:"key"`
	}{identity, key})
}

// Preflight every import while holding all source graph locks. Callers retain
// cleanup until execution and final verification finish. No new graph callback
// may begin if any requested source fails this preflight.
func prepareCommandReuses(ctx context.Context, scope Scope, spec Spec) (map[string]*commandReuse, func(), error) {
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
	type sourceGraph struct {
		dir        string
		nodes      map[string]workergraph.NodeState
		specs      map[string]Node
		checkpoint workergraph.Checkpoint
	}
	sources := map[string]sourceGraph{}
	for _, key := range keys {
		dir, err := existingCommandGraphDirectory(scope.RunDir, "graph-tools", key)
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
		checkpoint, specs, err := readCommandSnapshot(ctx, scope, key, dir)
		if err != nil {
			return fail(err)
		}
		if !slices.Contains([]string{"failed", "succeeded", "interrupted"}, checkpoint.Status) || len(checkpoint.Nodes) == 0 || len(checkpoint.Nodes) > 64 {
			return fail(errors.New("reuse source graph must be settled or interrupted; inspect its graph.json before recovery"))
		}
		source := sourceGraph{dir: dir, nodes: map[string]workergraph.NodeState{}, specs: specs, checkpoint: checkpoint}
		for _, node := range checkpoint.Nodes {
			source.nodes[node.ID] = node
		}
		sources[key] = source
	}
	verified := map[string]map[string]bool{}
	for _, node := range spec.Nodes {
		if node.Kind != "reuse" {
			continue
		}
		source := sources[node.ReuseFrom.Key]
		state, ok := source.nodes[node.ReuseFrom.Node]
		if !ok || state.Status != "succeeded" {
			return fail(errors.New("reuse source node must have succeeded"))
		}
		if err := validateInterruptedReuse(scope, source.checkpoint, source.specs, state.ID); err != nil {
			return fail(err)
		}
		// The snapshot above validates the entire source's structure and identity.
		// Only this dependency closure supplies the imported observation; unrelated
		// damaged artifacts or imports must not prevent independent salvage.
		closure := nodeAncestors(source.specs, state.ID)
		closure[state.ID] = true
		if verified[node.ReuseFrom.Key] == nil {
			verified[node.ReuseFrom.Key] = map[string]bool{}
		}
		for member := range closure {
			memberSpec := source.specs[member]
			if memberSpec.Kind == "reuse" || memberSpec.ReuseFrom != nil {
				return fail(errors.New("reuse dependency closure contains a recursive import"))
			}
			memberState := source.nodes[member]
			if memberState.Status == "succeeded" && !verified[node.ReuseFrom.Key][member] {
				if err := verifyCommandNodeOutput(ctx, filepath.Join(source.dir, "nodes", member), memberSpec, memberState.Output); err != nil {
					return fail(fmt.Errorf("reuse source node %s verification: %w", member, err))
				}
				verified[node.ReuseFrom.Key][member] = true
			}
		}
		result[node.ID] = &commandReuse{SourceNode: state, SourceSpec: source.specs[state.ID], SourceDir: filepath.Join(source.dir, "nodes", state.ID), receipt: ReuseReceipt{Key: node.ReuseFrom.Key, Node: state.ID, Kind: state.Kind, DefinitionSHA256: state.DefinitionSHA256, InputSHA256: state.InputSHA256,
			ExecutionStartedAt: state.StartedAt.UTC().Format(time.RFC3339Nano), ExecutionFinishedAt: state.FinishedAt.UTC().Format(time.RFC3339Nano)}}
	}
	return result, cleanup, nil
}

func (r *commandReuse) Output() workergraph.Output {
	var value Output
	_ = json.Unmarshal(r.SourceNode.Output.Value, &value)
	receipt := r.receipt
	value.ReusedFrom = &receipt
	raw, _ := json.Marshal(value)
	return workergraph.Output{Value: raw, Artifacts: slices.Clone(r.SourceNode.Output.Artifacts)}
}

func (r *commandReuse) Verify(ctx context.Context, out workergraph.Output) error {
	expected := r.Output()
	// Old receipts have no execution-window fields. Preserve their exact bytes
	// in checkpoints while checking every original identity and artifact field.
	var value Output
	if json.Unmarshal(out.Value, &value) == nil && value.ReusedFrom != nil && value.ReusedFrom.ExecutionStartedAt == "" && value.ReusedFrom.ExecutionFinishedAt == "" {
		var legacy Output
		_ = json.Unmarshal(expected.Value, &legacy)
		legacy.ReusedFrom.ExecutionStartedAt, legacy.ReusedFrom.ExecutionFinishedAt = "", ""
		expected.Value, _ = json.Marshal(legacy)
	}
	if !bytes.Equal(expected.Value, out.Value) || !slices.Equal(expected.Artifacts, out.Artifacts) {
		return errors.New("reused node output or provenance changed")
	}
	return verifyCommandNodeOutput(ctx, r.SourceDir, r.SourceSpec, r.SourceNode.Output)
}

// The source lock excludes a live graph runner. Declarations remain a trust
// boundary: this proves graph/resource independence, not shell isolation.
func validateInterruptedReuse(scope Scope, checkpoint workergraph.Checkpoint, specs map[string]Node, id string) error {
	states := make(map[string]workergraph.NodeState, len(checkpoint.Nodes))
	var unresolved []string
	for _, node := range checkpoint.Nodes {
		states[node.ID] = node
		if node.Status == "running" || node.Status == "pending" {
			unresolved = append(unresolved, node.ID)
		}
	}
	if len(unresolved) == 0 {
		return nil
	}
	closure := nodeAncestors(specs, id)
	closure[id] = true
	for member := range closure {
		if states[member].Status != "succeeded" {
			return fmt.Errorf("reuse %s requires a successful dependency subgraph; inspect node %s in graph.json and nodes/%s before recovery", id, member, member)
		}
		for _, unknown := range unresolved {
			if member == unknown || nodeAncestors(specs, member)[unknown] || nodeAncestors(specs, unknown)[member] || resourcesConflict(specs[member].Resources, specs[unknown].Resources, scope.Workspace) {
				return fmt.Errorf("reuse %s is influenced by unresolved node %s through dependencies or declared resources; inspect graph.json and nodes/%s; uncertain effects cannot be replayed", id, unknown, unknown)
			}
		}
	}
	return nil
}
