//go:build linux

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pwnmesh/internal/workergraph"
)

// Exercise the failure observed in the real model run: JSON was on stdout,
// while a Python diagnostic was correctly written to stderr.
func TestCommandGraphSeparateStreamsPreserveJSONDependencies(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	spec := commandGraphSpec{Key: "streams", Nodes: []commandGraphNode{
		{ID: "source", Command: `python3 -c 'import json,sys; sys.stderr.write("debug input\n"); print(json.dumps([2,3,5]))'`, Resources: []string{}},
		{ID: "sum", Command: `python3 -c 'import json,os,sys; deps={d["id"]:d for d in json.load(open(os.environ["PWNMESH_DEPENDENCIES"]))}; path=deps["source"]["output"]["value"]["output_path"]; values=json.load(open(path)); sys.stderr.write("left stdout="+path+"\n"); print(json.dumps({"values":values,"sum":sum(values)}))'`, DependsOn: []workergraph.Dependency{{ID: "source"}}, Resources: []string{}},
	}}
	cp, err := commandGraphCall(t, context.Background(), job, dir, spec)
	if err != nil || cp.Status != "succeeded" {
		t.Fatalf("stderr contaminated a JSON dependency: %+v %v", cp, err)
	}
	for _, id := range []string{"source", "sum"} {
		node, value := commandNodeValue(t, cp, id)
		stdout, outErr := os.ReadFile(value.OutputPath)
		stderr, errErr := os.ReadFile(value.StderrPath)
		if outErr != nil || errErr != nil || !json.Valid(stdout) || string(stdout) != value.Stdout || string(stderr) != value.Stderr || len(stderr) == 0 {
			t.Fatalf("%s lost stream identity: stdout=%q stderr=%q value=%+v errors=%v/%v", id, stdout, stderr, value, outErr, errErr)
		}
		if len(node.Output.Artifacts) != 2 || node.Output.Artifacts[0].Path != value.OutputPath || node.Output.Artifacts[1].Path != value.StderrPath {
			t.Fatalf("%s did not retain both stream receipts: %+v", id, node.Output)
		}
		if files := commandOutputFiles(node.Output); len(files) != 0 {
			t.Fatalf("reserved stream logs leaked into declared artifacts: %+v", files)
		}
	}
	_, value := commandNodeValue(t, cp, "sum")
	var receipt struct {
		Values []int `json:"values"`
		Sum    int   `json:"sum"`
	}
	if err := json.Unmarshal([]byte(value.Stdout), &receipt); err != nil || receipt.Sum != 10 || len(receipt.Values) != 3 || receipt.Values[0] != 2 || receipt.Values[1] != 3 || receipt.Values[2] != 5 {
		t.Fatalf("join did not compute from exact upstream stdout: %s %v", value.Stdout, err)
	}
}

func TestCommandGraphStderrCannotSatisfyStdoutCondition(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	spec := commandGraphSpec{Key: "stderr-condition", Nodes: []commandGraphNode{
		{ID: "source", Command: `printf ordinary; printf authorize >&2`, Resources: []string{}},
		{ID: "side-effect", Command: `touch "$PWNMESH_WORKSPACE/forbidden"`, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source"}}, When: &commandGraphWhen{Node: "source", Contains: "authorize"}},
	}}
	cp, err := commandGraphCall(t, context.Background(), job, dir, spec)
	if err != nil || cp.Status != "succeeded" {
		t.Fatalf("conditional graph failed: %+v %v", cp, err)
	}
	conditional, _ := commandNodeValue(t, cp, "side-effect")
	if conditional.Status != "skipped" {
		t.Fatalf("stderr authorized a stdout condition: %+v", conditional)
	}
	if _, err := os.Stat(filepath.Join(job.Workspace, "forbidden")); !os.IsNotExist(err) {
		t.Fatalf("conditional side effect executed: %v", err)
	}
}

func TestCommandGraphDoesNotCleanRealStdoutNoise(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	spec := commandGraphSpec{Key: "no-cleaning", Nodes: []commandGraphNode{{ID: "source", Command: `printf 'debug\n{"sum":10}\n'; printf diagnostic >&2`, Resources: []string{}}}}
	cp, err := commandGraphCall(t, context.Background(), job, dir, spec)
	if err != nil {
		t.Fatal(err)
	}
	_, value := commandNodeValue(t, cp, "source")
	stdout, err := os.ReadFile(value.OutputPath)
	if err != nil || string(stdout) != "debug\n{\"sum\":10}\n" || value.Stdout != string(stdout) || json.Valid(stdout) || value.Stderr != "diagnostic" {
		t.Fatalf("runtime discarded or reclassified stdout bytes: %+v %q %v", value, stdout, err)
	}
}

func TestCommandGraphRetainsFailureDiagnosticsAndBoundsStderrPreview(t *testing.T) {
	for _, test := range []struct {
		name, command string
		failed        bool
		stderrSize    int
	}{
		{"failure", `printf '{"partial":true}\n'; printf 'failure reason\n' >&2; exit 7`, true, len("failure reason\n")},
		{"empty", `printf '{"ok":true}\n'`, false, 0},
		{"truncated", `printf '{"ok":true}\n'; head -c 9001 /dev/zero | tr '\0' x >&2`, false, 9001},
	} {
		t.Run(test.name, func(t *testing.T) {
			job, dir := graphWrapperJob(t), t.TempDir()
			spec := commandGraphSpec{Key: test.name, Nodes: []commandGraphNode{{ID: "source", Command: test.command, Resources: []string{}}}}
			cp, err := commandGraphCall(t, context.Background(), job, dir, spec)
			if (err != nil) != test.failed {
				t.Fatalf("unexpected command result: %+v %v", cp, err)
			}
			node, value := commandNodeValue(t, cp, "source")
			if test.failed {
				// Failed callbacks retain raw files but do not publish an
				// unverified Output as a successful dependency receipt.
				nodeDir := filepath.Join(dir, "graph-tools", test.name, "nodes", "source")
				stdoutPath, stderrPath := filepath.Join(nodeDir, "stdout.log"), filepath.Join(nodeDir, "stderr.log")
				stdout, outErr := os.ReadFile(stdoutPath)
				stderr, errErr := os.ReadFile(stderrPath)
				if outErr != nil || errErr != nil || string(stdout) != "{\"partial\":true}\n" || string(stderr) != "failure reason\n" {
					t.Fatalf("failed command lost raw streams: stdout=%q stderr=%q errors=%v/%v", stdout, stderr, outErr, errErr)
				}
				if node.Status != "failed" || len(node.Output.Value) != 0 || !strings.Contains(node.Error, "command exited 7") || !strings.Contains(node.Error, stdoutPath) || !strings.Contains(node.Error, stderrPath) {
					t.Fatalf("failed command lost diagnostic paths or published unverified output: %+v", node)
				}
				return
			}
			stderr, readErr := os.ReadFile(value.StderrPath)
			if readErr != nil || len(stderr) != test.stderrSize || !json.Valid([]byte(value.Stdout)) || len(node.Output.Artifacts) != 2 || node.Output.Artifacts[1].Path != value.StderrPath {
				t.Fatalf("diagnostics or exact stdout lost: %+v bytes=%d error=%v", value, len(stderr), readErr)
			}
			if test.stderrSize > 8000 {
				if len(value.Stderr) > 8200 || !strings.Contains(strings.ToLower(value.Stderr), "truncated") {
					t.Fatalf("stderr preview is not bounded: %d bytes", len(value.Stderr))
				}
				stderr[len(stderr)-1] = 'y'
				if err := os.WriteFile(value.StderrPath, stderr, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := commandGraphCall(t, context.Background(), job, dir, spec); err == nil {
					t.Fatal("stderr changes past preview were not verified")
				}
			}
		})
	}
}

func TestCommandGraphRejectsStderrTamperingOnRestoreAndReuse(t *testing.T) {
	for _, mode := range []string{"restore", "reuse"} {
		for _, mutation := range []string{"bytes", "missing", "preview", "path", "downgrade", "symlink"} {
			t.Run(mode+"/"+mutation, func(t *testing.T) {
				job, dir := graphWrapperJob(t), t.TempDir()
				spec := commandGraphSpec{Key: "source", Nodes: []commandGraphNode{{ID: "original", Command: `printf 'once\n' >> "$PWNMESH_WORKSPACE/effects"; printf '{"ok":true}\n'; printf 'original diagnostic\n' >&2`, Resources: []string{}}}}
				if _, err := commandGraphCall(t, context.Background(), job, dir, spec); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "graph-tools", "source", "graph.json")
				raw, err := os.ReadFile(path)
				var cp workergraph.Checkpoint
				if err != nil || json.Unmarshal(raw, &cp) != nil || len(cp.Nodes) != 1 {
					t.Fatal("missing durable source checkpoint")
				}
				var value commandGraphOutput
				if err := json.Unmarshal(cp.Nodes[0].Output.Value, &value); err != nil || value.StderrPath == "" {
					t.Fatalf("missing stderr receipt: %+v %v", value, err)
				}
				switch mutation {
				case "bytes":
					err = os.WriteFile(value.StderrPath, []byte("changed\n"), 0600)
				case "missing":
					err = os.Remove(value.StderrPath)
				case "preview":
					value.Stderr = "forged diagnostic preview"
				case "path":
					value.StderrPath = value.OutputPath
				case "downgrade":
					// Removing both metadata and artifact must not turn a new
					// command into a historical merged-log receipt.
					value.Stderr, value.StderrPath = "", ""
					cp.Nodes[0].Output.Artifacts = cp.Nodes[0].Output.Artifacts[:len(cp.Nodes[0].Output.Artifacts)-1]
				case "symlink":
					if err = os.Remove(value.StderrPath); err == nil {
						err = os.Symlink(value.OutputPath, value.StderrPath)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				cp.Nodes[0].Output.Value, _ = json.Marshal(value)
				raw, _ = json.Marshal(cp)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				call := spec
				if mode == "reuse" {
					call = commandGraphSpec{Key: "reuse", Nodes: []commandGraphNode{
						{ID: "imported", Kind: "reuse", ReuseFrom: &commandGraphReuse{Key: "source", Node: "original"}, Resources: []string{}},
						{ID: "new-effect", Command: `touch "$PWNMESH_WORKSPACE/forbidden"`, Resources: []string{}},
					}}
				}
				if _, err := commandGraphCall(t, context.Background(), job, dir, call); err == nil {
					t.Fatal("changed stderr was accepted")
				}
				effects, err := os.ReadFile(filepath.Join(job.Workspace, "effects"))
				if err != nil || string(effects) != "once\n" {
					t.Fatalf("failed verification replayed the producer: %q %v", effects, err)
				}
				if _, err := os.Stat(filepath.Join(job.Workspace, "forbidden")); !os.IsNotExist(err) {
					t.Fatalf("new work ran before source verification: %v", err)
				}
			})
		}
	}
}

func TestCommandGraphRejectsLegacyCombinedStreamsWithoutReplay(t *testing.T) {
	for _, mode := range []string{"restore", "reuse"} {
		t.Run(mode, func(t *testing.T) {
			job, dir := graphWrapperJob(t), t.TempDir()
			spec := commandGraphSpec{Key: "legacy", Nodes: []commandGraphNode{{ID: "source", Command: `printf 'once\n' >> "$PWNMESH_WORKSPACE/effects"; printf '{"ok":true}\n'; printf diagnostic >&2`, Resources: []string{}}}}
			if _, err := commandGraphCall(t, context.Background(), job, dir, spec); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "graph-tools", "legacy", "graph.json")
			raw, err := os.ReadFile(path)
			var cp workergraph.Checkpoint
			if err != nil || json.Unmarshal(raw, &cp) != nil {
				t.Fatal("missing saved graph")
			}
			if cp.Version != "mixed-dag-v3" {
				t.Fatalf("split-stream semantics were not versioned: %q", cp.Version)
			}
			cp.Version = "mixed-dag-v2"
			raw, _ = json.Marshal(cp)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			call := spec
			if mode == "reuse" {
				call = commandGraphSpec{Key: "import-old", Nodes: []commandGraphNode{{ID: "old", Kind: "reuse", ReuseFrom: &commandGraphReuse{Key: "legacy", Node: "source"}, Resources: []string{}}}}
			}
			if _, err := commandGraphCall(t, context.Background(), job, dir, call); err == nil || !strings.Contains(err.Error(), "mismatch") {
				t.Fatalf("legacy stream semantics were silently accepted: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatalf("legacy checkpoint was rewritten: %v", err)
			}
			effects, err := os.ReadFile(filepath.Join(job.Workspace, "effects"))
			if err != nil || string(effects) != "once\n" {
				t.Fatalf("legacy command was replayed: %q %v", effects, err)
			}
		})
	}
}

func TestCommandGraphReservedStderrDeclarationsFailBeforeExecution(t *testing.T) {
	for _, declaration := range []string{"artifact", "input"} {
		t.Run(declaration, func(t *testing.T) {
			job, dir := graphWrapperJob(t), t.TempDir()
			spec := commandGraphSpec{Key: "reserved-stderr", Nodes: []commandGraphNode{
				{ID: "producer", Command: `touch "$PWNMESH_WORKSPACE/producer-started"; printf diagnostic >&2`, Resources: []string{}},
				{ID: "consumer", Command: `touch "$PWNMESH_WORKSPACE/consumer-started"`, Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "producer"}}},
			}}
			if declaration == "artifact" {
				spec.Nodes[0].Artifacts = []string{"stderr.log"}
			} else {
				spec.Nodes[1].Inputs = []commandGraphInput{{Node: "producer", Artifact: "stderr.log"}}
			}
			if _, err := commandGraphCall(t, context.Background(), job, dir, spec); err == nil {
				t.Fatal("reserved stderr log accepted as a declared file")
			}
			for _, name := range []string{"producer-started", "consumer-started"} {
				if _, err := os.Stat(filepath.Join(job.Workspace, name)); !os.IsNotExist(err) {
					t.Fatalf("invalid declaration allowed side effects: %s: %v", name, err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "graph-tools", spec.Key, "graph.json")); !os.IsNotExist(err) {
				t.Fatalf("invalid declaration created an execution checkpoint: %v", err)
			}
		})
	}
}

func TestCommandGraphReusePreservesNonemptyStderrWithoutReplay(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	source := commandGraphSpec{Key: "source-streams", Nodes: []commandGraphNode{{
		ID: "source", Command: `printf 'once\n' >> "$PWNMESH_WORKSPACE/effects"; printf '{"ok":true}\n' > result.json; cat result.json; printf 'original diagnostic\n' >&2`, Resources: []string{}, Artifacts: []string{"result.json"},
	}}}
	cp, err := commandGraphCall(t, context.Background(), job, dir, source)
	if err != nil || cp.Status != "succeeded" {
		t.Fatalf("source failed: %+v %v", cp, err)
	}
	original, originalValue := commandNodeValue(t, cp, "source")
	sourcePath := filepath.Join(dir, "graph-tools", source.Key, "graph.json")
	before, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	reuse := commandGraphSpec{Key: "reuse-streams", Nodes: []commandGraphNode{{ID: "imported", Kind: "reuse", ReuseFrom: &commandGraphReuse{Key: source.Key, Node: "source"}, Resources: []string{}}}}
	// The second call recovers the completed import and reverifies its source.
	for attempt := 0; attempt < 2; attempt++ {
		cp, err := commandGraphCall(t, context.Background(), job, dir, reuse)
		if err != nil || cp.Status != "succeeded" {
			t.Fatalf("reuse attempt %d failed: %+v %v", attempt, cp, err)
		}
		imported, value := commandNodeValue(t, cp, "imported")
		if value.ReusedFrom == nil || value.ReusedFrom.Key != source.Key || value.ReusedFrom.Node != "source" || value.ReusedFrom.DefinitionSHA256 != original.DefinitionSHA256 || value.ReusedFrom.InputSHA256 != original.InputSHA256 {
			t.Fatalf("source provenance lost: %+v", value)
		}
		if value.Stdout != originalValue.Stdout || value.OutputPath != originalValue.OutputPath || value.Stderr != "original diagnostic\n" || value.Stderr != originalValue.Stderr || value.StderrPath != originalValue.StderrPath || !slices.Equal(imported.Output.Artifacts, original.Output.Artifacts) {
			t.Fatalf("reuse changed stream previews, paths or artifact receipts: %+v original=%+v", imported.Output, original.Output)
		}
		if len(imported.Output.Artifacts) != 3 || imported.Output.Artifacts[1].Path != filepath.Join(filepath.Dir(value.OutputPath), "result.json") || imported.Output.Artifacts[2].Path != value.StderrPath {
			t.Fatalf("reuse changed declared file offsets or stderr receipt: %+v", imported.Output.Artifacts)
		}
		stderr, err := os.ReadFile(value.StderrPath)
		if err != nil || string(stderr) != value.Stderr {
			t.Fatalf("retained stderr differs from its reuse preview: %q %v", stderr, err)
		}
	}
	after, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("reuse rewrote source checkpoint: %v", err)
	}
	effects, err := os.ReadFile(filepath.Join(job.Workspace, "effects"))
	if err != nil || string(effects) != "once\n" {
		t.Fatalf("reuse replayed source command: %q %v", effects, err)
	}
}
