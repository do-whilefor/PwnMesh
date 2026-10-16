//go:build linux

package workergraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestValidateSnapshotIsReadOnlyAndChecksBindings(t *testing.T) {
	calls := 0
	definition := Definition{Version: "snapshot-v1", Nodes: []Node{
		{ID: "a", Kind: "function", Input: json.RawMessage(`{"task":"observe"}`), Run: func(context.Context, Input) (Output, error) {
			calls++
			return Output{Value: json.RawMessage(`{"observation":"first"}`)}, nil
		}, Verify: func(context.Context, Input, Output) error { calls++; return nil }},
		{ID: "b", Kind: "function", DependsOn: []Dependency{{ID: "a"}}, Run: func(context.Context, Input) (Output, error) {
			calls++
			return Output{}, errors.New("expected failure")
		}},
	}}
	options := Options{RunID: "snapshot-test", Input: json.RawMessage(`{"identity":"original"}`), Dir: t.TempDir(), Parallelism: 1}
	saved, err := Run(context.Background(), definition, options)
	if err == nil || saved.Status != "failed" {
		t.Fatalf("expected terminal failed source: %+v %v", saved, err)
	}
	before, err := os.ReadFile(filepath.Join(options.Dir, "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	priorCalls := calls
	if err := ValidateSnapshot(definition, options, saved); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(options.Dir, "graph.json"))
	if calls != priorCalls || !bytes.Equal(before, after) {
		t.Fatal("snapshot validation invoked callbacks or changed the checkpoint")
	}
	cases := []struct {
		name string
		edit func(*Checkpoint)
	}{
		{"run", func(c *Checkpoint) { c.RunID = "other-run" }},
		{"initial", func(c *Checkpoint) { c.InputSHA256 = strings.Repeat("0", 64) }},
		{"definition", func(c *Checkpoint) { c.DefinitionSHA256 = strings.Repeat("0", 64) }},
		{"node-definition", func(c *Checkpoint) { c.Nodes[0].DefinitionSHA256 = "" }},
		{"bound-result", func(c *Checkpoint) { c.Nodes[0].Output.Value = json.RawMessage(`{"observation":"different"}`) }},
		{"timing", func(c *Checkpoint) {
			c.Nodes[0].FinishedAt = time.Time{}
		}},
		{"node-input", func(c *Checkpoint) { c.Nodes[1].InputSHA256 = strings.Repeat("0", 64) }},
		{"node-count", func(c *Checkpoint) { c.Nodes = c.Nodes[:1] }},
		{"relative-artifact", func(c *Checkpoint) {
			c.Nodes[0].Output.Artifacts = []Artifact{{Path: "relative", SHA256: strings.Repeat("0", 64)}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var changed Checkpoint
			if err := json.Unmarshal(before, &changed); err != nil {
				t.Fatal(err)
			}
			tc.edit(&changed)
			if err := ValidateSnapshot(definition, options, changed); err == nil {
				t.Fatal("changed source accepted")
			}
		})
	}
}

func TestCallbackInputReceiptSurvivesRecoveryTimingChanges(t *testing.T) {
	var binding string
	calls := 0
	definition := Definition{Version: "receipt-v1", Nodes: []Node{
		{ID: "source", Kind: "function", Run: successful, Verify: verified},
		{ID: "child", Kind: "agent", DependsOn: []Dependency{{ID: "source"}},
			Run: func(_ context.Context, input Input) (Output, error) {
				calls++
				binding = input.SHA256
				return Output{}, ErrInterrupted
			},
			Verify: verified,
			Reconcile: func(_ context.Context, input Input, saved NodeState) (Output, error) {
				if len(binding) != 64 || input.SHA256 != binding || saved.InputSHA256 != binding {
					return Output{}, errors.New("input receipt changed during recovery")
				}
				return Output{}, nil
			}},
	}}
	options := fixtureOptions(t, 1)
	if _, err := Run(context.Background(), definition, options); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("expected interruption: %v", err)
	}
	checkpoint, err := Run(context.Background(), definition, options)
	if err != nil || checkpoint.Status != "succeeded" || calls != 1 {
		t.Fatalf("receipt recovery failed or replayed work: %+v %v calls=%d", checkpoint, err, calls)
	}
}

func TestSavedNodeValidationRejectsBeforeCallbacks(t *testing.T) {
	definition := Definition{Version: "saved-node-v1", Nodes: []Node{{ID: "source", Kind: "function", Run: successful, Verify: verified}}}
	options := fixtureOptions(t, 1)
	if _, err := Run(context.Background(), definition, options); err != nil {
		t.Fatal(err)
	}
	original := checkpointBytes(t, options)
	callbacks := 0
	definition.Nodes[0].Run = func(context.Context, Input) (Output, error) { callbacks++; return Output{}, nil }
	definition.Nodes[0].Verify = func(context.Context, Input, Output) error { callbacks++; return nil }
	for _, tc := range []struct {
		name string
		edit func(*NodeState)
	}{
		{"kind", func(n *NodeState) { n.Kind = "other" }},
		{"negative-attempt", func(n *NodeState) { n.Attempt = -1 }},
		{"second-attempt", func(n *NodeState) { n.Attempt = 2 }},
		{"unknown-status", func(n *NodeState) { n.Status = "unknown" }},
		{"missing-start", func(n *NodeState) { n.StartedAt = time.Time{} }},
		{"missing-finish", func(n *NodeState) { n.FinishedAt = time.Time{} }},
		{"running-without-attempt", func(n *NodeState) { n.Status, n.Attempt = "running", 0 }},
		{"pending-with-attempt", func(n *NodeState) { n.Status = "pending" }},
		{"skipped-with-attempt", func(n *NodeState) { n.Status, n.Reason = "skipped", "condition" }},
		{"blocked-with-attempt", func(n *NodeState) { n.Status = "blocked" }},
		{"relative-artifact", func(n *NodeState) {
			n.Output.Artifacts = []Artifact{{Path: "relative", SHA256: strings.Repeat("0", 64)}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changed Checkpoint
			if err := json.Unmarshal(original, &changed); err != nil {
				t.Fatal(err)
			}
			tc.edit(&changed.Nodes[0])
			raw, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(options.Dir, "graph.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := ValidateSnapshot(definition, options, changed); err == nil {
				t.Fatal("snapshot accepted malformed node")
			}
			if _, err := Run(context.Background(), definition, options); err == nil {
				t.Fatal("resume accepted malformed node")
			}
			if callbacks != 0 || !bytes.Equal(raw, checkpointBytes(t, options)) {
				t.Fatal("rejected node invoked callbacks or rewrote its checkpoint")
			}
		})
	}
}

func TestResumeValidatesAllRetainedBindingsBeforeCallbacks(t *testing.T) {
	for _, status := range []string{"failed", "succeeded"} {
		for _, binding := range []string{"", strings.Repeat("0", 64)} {
			t.Run(status+"/"+binding, func(t *testing.T) {
				options := fixtureOptions(t, 1)
				options.Extend = true
				callbacks := 0
				verify := func(context.Context, Input, Output) error { callbacks++; return nil }
				definition := Definition{Version: "binding-preflight-v1", Nodes: []Node{
					{ID: "first", Kind: "function", Run: successful, Verify: verify},
					{ID: "target", Kind: "function", Optional: true, DependsOn: []Dependency{{ID: "first"}}, Verify: verify,
						Run: func(context.Context, Input) (Output, error) {
							if status == "failed" {
								return Output{Value: json.RawMessage(`"retained failure diagnostics"`)}, errors.New("optional failure")
							}
							return Output{}, nil
						}},
				}}
				saved, err := Run(context.Background(), definition, options)
				if err != nil || stateByID(saved, "target").Status != status {
					t.Fatalf("unexpected initial graph: %+v %v", saved, err)
				}
				for i := range saved.Nodes {
					if saved.Nodes[i].ID == "target" {
						saved.Nodes[i].InputSHA256 = binding
					}
				}
				raw, err := json.Marshal(saved)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(options.Dir, "graph.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := ValidateSnapshot(definition, options, saved); err == nil {
					t.Fatal("snapshot accepted an invalid retained binding")
				}
				definition.Nodes = append(definition.Nodes, Node{ID: "consumer", Kind: "function", DependsOn: []Dependency{{ID: "target", Optional: true}},
					Run: func(context.Context, Input) (Output, error) { callbacks++; return Output{}, nil }})
				callbacks = 0
				_, err = Run(context.Background(), definition, options)
				if err == nil || !strings.Contains(err.Error(), "input binding changed") || callbacks != 0 || !bytes.Equal(raw, checkpointBytes(t, options)) {
					t.Fatalf("invalid binding reached recovery or extension: err=%v callbacks=%d checkpoint_changed=%t", err, callbacks, !bytes.Equal(raw, checkpointBytes(t, options)))
				}
			})
		}
	}
}

func TestNodeDefinitionSHA256MatchesRunWithoutMutatingDependencies(t *testing.T) {
	node := Node{ID: "join", Kind: "function", DependsOn: []Dependency{{ID: "a_1"}, {ID: "a.1"}, {ID: "a-1"}, {ID: "a"}, {ID: "A"}, {ID: "1"}}, Run: func(context.Context, Input) (Output, error) { return Output{}, nil }}
	original := slices.Clone(node.DependsOn)
	digest := NodeDefinitionSHA256(node)
	if !reflect.DeepEqual(node.DependsOn, original) {
		t.Fatal("hashing reordered caller-owned dependencies")
	}
	definition := Definition{Version: "v1", Nodes: []Node{node}}
	for _, dependency := range node.DependsOn {
		definition.Nodes = append(definition.Nodes, Node{ID: dependency.ID, Kind: "function", Run: node.Run})
	}
	options := Options{RunID: "test", Dir: t.TempDir(), Parallelism: 2}
	checkpoint, err := Run(context.Background(), definition, options)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, state := range checkpoint.Nodes {
		ids = append(ids, state.ID)
		if state.ID == "join" && state.DefinitionSHA256 != digest {
			t.Fatal("exported node hash differs from Run")
		}
	}
	if !reflect.DeepEqual(ids, []string{"1", "A", "a", "a-1", "a.1", "a_1", "join"}) {
		t.Fatalf("checkpoint ID order changed: %v", ids)
	}
	slices.Reverse(node.DependsOn)
	slices.Reverse(definition.Nodes)
	if NodeDefinitionSHA256(node) != digest {
		t.Fatal("dependency permutation changed node identity")
	}
	if err := ValidateSnapshot(definition, options, checkpoint); err != nil {
		t.Fatalf("definition permutation invalidated saved snapshot: %v", err)
	}
	for i := range definition.Nodes {
		definition.Nodes[i].Verify = verified
		definition.Nodes[i].Run = func(context.Context, Input) (Output, error) {
			return Output{}, errors.New("accepted nodes must not replay after definition permutation")
		}
	}
	resumed, err := Run(context.Background(), definition, options)
	if err != nil || resumed.Status != "succeeded" || resumed.DefinitionSHA256 != checkpoint.DefinitionSHA256 {
		t.Fatalf("definition permutation changed persisted graph identity or replayed work: %+v %v", resumed, err)
	}
}

func TestNodeDefinitionSHA256NormalizesEmptyInputsAndDependencies(t *testing.T) {
	var canonical, canonicalGraph string
	for _, input := range []struct {
		name  string
		value json.RawMessage
	}{{"nil", nil}, {"empty", json.RawMessage{}}} {
		for _, dependencies := range []struct {
			name  string
			value []Dependency
		}{{"nil", nil}, {"empty", []Dependency{}}} {
			t.Run(input.name+"-input-"+dependencies.name+"-dependencies", func(t *testing.T) {
				node := Node{ID: "source", Kind: "function", Input: input.value, DependsOn: dependencies.value, Run: func(context.Context, Input) (Output, error) { return Output{}, nil }}
				digest := NodeDefinitionSHA256(node)
				checkpoint, err := Run(context.Background(), Definition{Version: "v1", Nodes: []Node{node}}, Options{RunID: "test", Dir: t.TempDir(), Parallelism: 1})
				if err != nil || checkpoint.Nodes[0].DefinitionSHA256 != digest {
					t.Fatalf("definition manifest hash differs from durable receipt: digest=%s checkpoint=%+v err=%v", digest, checkpoint, err)
				}
				if canonical == "" {
					canonical = digest
					canonicalGraph = checkpoint.DefinitionSHA256
				} else if canonical != digest {
					t.Fatal("nil and empty inputs/dependencies have different definition hashes")
				}
				if canonicalGraph != checkpoint.DefinitionSHA256 {
					t.Fatal("nil and empty inputs/dependencies have different graph hashes")
				}
			})
		}
	}
}

func TestResumeRejectsCheckpointSymlinkWithoutCallbacks(t *testing.T) {
	options := fixtureOptions(t, 1)
	definition := Definition{Version: "v1", Nodes: []Node{{ID: "one", Kind: "function", Run: successful, Verify: verified}}}
	if _, err := Run(context.Background(), definition, options); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(options.Dir, "graph.json")
	retained := filepath.Join(options.Dir, "retained.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, retained); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(retained, path); err != nil {
		t.Fatal(err)
	}
	calls := 0
	definition.Nodes[0].Verify = func(context.Context, Input, Output) error { calls++; return nil }
	if _, err := Run(context.Background(), definition, options); err == nil || calls != 0 {
		t.Fatalf("checkpoint alias reached recovery: err=%v calls=%d", err, calls)
	}
	after, err := os.ReadFile(retained)
	info, statErr := os.Lstat(path)
	if err != nil || statErr != nil || !bytes.Equal(before, after) || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("rejected checkpoint alias was rewritten")
	}
}

func TestFreshRunRejectsCheckpointDirectoryAliasBeforeCallbacks(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "retained")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}
	options := fixtureOptions(t, 1)
	options.Dir = alias
	calls := 0
	definition := Definition{Version: "v1", Nodes: []Node{{ID: "one", Kind: "function", Run: func(context.Context, Input) (Output, error) { calls++; return Output{}, nil }}}}
	if _, err := Run(context.Background(), definition, options); err == nil || calls != 0 {
		t.Fatalf("fresh run wrote through a directory alias: err=%v calls=%d", err, calls)
	}
	entries, err := os.ReadDir(realDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected directory alias acquired graph state: entries=%v err=%v", entries, err)
	}
}

func TestResumeRejectsCheckpointFIFOWithoutWaitingForWriter(t *testing.T) {
	options := fixtureOptions(t, 1)
	path := filepath.Join(options.Dir, "graph.json")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	definition := Definition{Version: "v1", Nodes: []Node{{ID: "one", Kind: "function", Run: func(context.Context, Input) (Output, error) { calls++; return Output{}, nil }}}}
	done := make(chan error, 1)
	go func() { _, err := Run(ctx, definition, options); done <- err }()
	select {
	case err := <-done:
		if err == nil || calls != 0 {
			t.Fatalf("nonregular checkpoint accepted: err=%v calls=%d", err, calls)
		}
	case <-time.After(250 * time.Millisecond):
		cancel()
		// Release a broken blocking loader so the regression never leaks a goroutine.
		writer, err := os.OpenFile(path, os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := writer.Write([]byte(`{}`))
		closeErr := writer.Close()
		<-done
		t.Fatalf("resume blocked on FIFO despite cancellation: write=%v close=%v", writeErr, closeErr)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("rejected checkpoint FIFO was rewritten")
	}
}

func TestResumePreservesLargeAcceptedSkipReason(t *testing.T) {
	options := fixtureOptions(t, 1)
	// JSON HTML escaping expands this accepted reason beyond 8 MiB on disk.
	reason := strings.Repeat("<", (8<<20)/6+1)
	definition := Definition{Version: "v1", Nodes: []Node{{ID: "one", Kind: "function", Run: successful,
		When: func(context.Context, Input) (bool, string, error) { return false, reason, nil }}}}
	if _, err := Run(context.Background(), definition, options); err != nil {
		t.Fatal(err)
	}
	saved, err := Run(context.Background(), definition, options)
	if err != nil || saved.Nodes[0].Status != "skipped" || saved.Nodes[0].Reason != reason {
		t.Fatalf("accepted large diagnostic could not resume: err=%v", err)
	}
}

func TestCheckpointReaderRejectsInvalidStateWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"trailing-value", `{} {}`},
		{"trailing-garbage", `{} garbage`},
		{"truncated", `{"schema_version":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "graph.json")
			if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadCheckpoint(context.Background(), dir); err == nil {
				t.Fatal("invalid checkpoint JSON accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != tc.raw {
				t.Fatal("invalid checkpoint was rewritten")
			}
		})
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "graph.json")
	if err := os.Symlink(filepath.Join(dir, "missing.json"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCheckpoint(context.Background(), dir); err == nil || os.IsNotExist(err) {
		t.Fatalf("dangling checkpoint alias was treated as a fresh graph: %v", err)
	}
}

func TestCheckpointReaderHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadCheckpoint(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read accessed the checkpoint: %v", err)
	}
	file, err := os.CreateTemp(t.TempDir(), "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(strings.Repeat("x", 128<<10)); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	reader := checkpointReader{ctx: ctx, file: file}
	buffer := make([]byte, 128<<10)
	if n, err := reader.Read(buffer); err != nil || n != 64<<10 {
		t.Fatalf("checkpoint read did not bound its cancellation interval: n=%d err=%v", n, err)
	}
	cancel()
	if n, err := reader.Read(buffer); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("checkpoint read continued after cancellation: n=%d err=%v", n, err)
	}
}

type checkpointReadContext struct {
	context.Context
	check func() error
}

func (c checkpointReadContext) Err() error { return c.check() }

func TestResumeNeverTreatsDisappearingOpenCheckpointAsNew(t *testing.T) {
	options := fixtureOptions(t, 1)
	path := filepath.Join(options.Dir, "graph.json")
	definition := Definition{Version: "v1", Nodes: []Node{{ID: "one", Kind: "function", Run: successful, Verify: verified}}}
	if _, err := Run(context.Background(), definition, options); err != nil {
		t.Fatal(err)
	}
	calls, contextChecks := 0, 0
	definition.Nodes[0].Run = func(context.Context, Input) (Output, error) { calls++; return Output{}, nil }
	definition.Nodes[0].Verify = func(context.Context, Input, Output) error { calls++; return nil }
	var removeErr error
	ctx := checkpointReadContext{Context: context.Background(), check: func() error {
		contextChecks++
		// Run and ReadCheckpoint each check cancellation before opening. The
		// third check is the first read from the already opened descriptor.
		if contextChecks == 3 {
			removeErr = os.Remove(path)
		}
		return nil
	}}
	if _, err := Run(ctx, definition, options); err == nil || errors.Is(err, os.ErrNotExist) || calls != 0 || removeErr != nil {
		t.Fatalf("disappearing checkpoint authorized fresh work: err=%v calls=%d remove=%v", err, calls, removeErr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("disappearing checkpoint was recreated")
	}
}
