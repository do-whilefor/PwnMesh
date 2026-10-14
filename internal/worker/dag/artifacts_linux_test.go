//go:build linux

package dag

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/workergraph"
)

func artifactContractSpec() Spec {
	return Spec{Key: "contracts", Nodes: []Node{
		{ID: "source", Command: `printf '{"ok":true}' > result.json; printf 'diagnostic, not JSON\n'`, Resources: []string{}, Artifacts: []string{"result.json"}},
		{ID: "consume", Command: "printf consumed", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "source"}}, Inputs: []Input{{Node: "source", Artifact: "result.json"}}},
	}}
}

func TestArtifactContractRejectsUndeclaredInputsBeforeAnyExecution(t *testing.T) {
	job, dir := graphScope(t), t.TempDir()
	spec := artifactContractSpec()
	spec.Nodes[0].Artifacts = nil
	spec.Nodes[0].Command = `touch "$PWNMESH_WORKSPACE/should-not-start"`
	_, err := commandGraphCall(t, context.Background(), job, dir, spec)
	if err == nil || !strings.Contains(err.Error(), "undeclared") || !strings.Contains(err.Error(), "result.json") {
		t.Fatalf("missing declaration was not explained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(job.Workspace, "should-not-start")); !os.IsNotExist(err) {
		t.Fatal("invalid contract allowed a producer side effect")
	}
	if _, err := os.Stat(filepath.Join(dir, "graph-tools", spec.Key, "graph.json")); !os.IsNotExist(err) {
		t.Fatal("invalid contract created a graph execution checkpoint")
	}
}

func TestArtifactContractPreflight(t *testing.T) {
	for name, alter := range map[string]func(*Spec){
		"unknown producer":    func(s *Spec) { s.Nodes[1].Inputs[0].Node = "missing" },
		"indirect dependency": func(s *Spec) { s.Nodes[1].DependsOn = nil },
		"optional dependency": func(s *Spec) { s.Nodes[1].DependsOn[0].Optional = true },
		"undeclared artifact": func(s *Spec) { s.Nodes[0].Artifacts = nil },
		"reserved log":        func(s *Spec) { s.Nodes[1].Inputs[0].Artifact = "stdout.log" },
		"reserved session":    func(s *Spec) { s.Nodes[1].Inputs[0].Artifact = "session.json" },
		"path traversal":      func(s *Spec) { s.Nodes[1].Inputs[0].Artifact = "../result.json" },
		"duplicate":           func(s *Spec) { s.Nodes[1].Inputs = append(s.Nodes[1].Inputs, s.Nodes[1].Inputs[0]) },
		"too many inputs":     func(s *Spec) { s.Nodes[1].Inputs = make([]Input, 65) },
	} {
		t.Run(name, func(t *testing.T) {
			spec := artifactContractSpec()
			alter(&spec)
			if err := validateCommandInputs(&spec); err == nil {
				t.Fatal("invalid input contract accepted")
			}
		})
	}
	spec := artifactContractSpec()
	if err := validateCommandInputs(&spec); err != nil {
		t.Fatalf("valid input contract rejected: %v", err)
	}
	spec.Nodes[0].Artifacts, spec.Nodes[1].Inputs = nil, nil
	if err := validateCommandInputs(&spec); err != nil {
		t.Fatalf("legacy log-only graph rejected: %v", err)
	}
}

func TestArtifactContractProjectionPreservesDurableReceipts(t *testing.T) {
	dir := t.TempDir()
	value, _ := json.Marshal(Output{Stdout: "not JSON", OutputPath: filepath.Join(dir, "stdout.log")})
	state := workergraph.NodeState{ID: "source", Kind: "function", Status: "succeeded", Attempt: 1, InputSHA256: "input-binding", Output: workergraph.Output{Value: value}}
	for _, name := range []string{"stdout.log", "dependencies.json", "events.jsonl", "session.json", "result.json", "nested/result.json", "../outside.json"} {
		state.Output.Artifacts = append(state.Output.Artifacts, workergraph.Artifact{Path: filepath.Join(dir, name), SHA256: strings.Repeat("a", 64)})
	}
	before, _ := json.Marshal(state)
	views := commandGraphViews([]workergraph.NodeState{state})
	if len(views) != 1 || len(views[0].Output.Files) != 2 || views[0].Output.Files["result.json"].Path != filepath.Join(dir, "result.json") || views[0].Output.Files["nested/result.json"].Path != filepath.Join(dir, "nested", "result.json") {
		t.Fatalf("named files contain logs, reserved paths or outside files: %+v", views)
	}
	raw, err := json.Marshal(views[0])
	var decoded workergraph.NodeState
	if err != nil || json.Unmarshal(raw, &decoded) != nil || !reflect.DeepEqual(state, decoded) {
		t.Fatalf("view changed durable receipt fields: %s %v", raw, err)
	}
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		t.Fatal("projection mutated original checkpoint output")
	}
	if got, _ := json.Marshal(commandGraphViews(nil)); string(got) != "[]" {
		t.Fatalf("empty dependency array changed: %s", got)
	}
}

func TestArtifactContractChecksRetainedFileBeforeConsumption(t *testing.T) {
	for _, scenario := range []string{"changed", "missing", "symlink", "failed producer", "missing receipt", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range map[string]string{"stdout.log": "diagnostic", "result.json": `{"ok":true}`} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := commandNodeOutput(context.Background(), dir, []string{"result.json"})
			if err != nil {
				t.Fatal(err)
			}
			deps := []workergraph.NodeState{{ID: "source", Status: "succeeded", Output: out}}
			consumer := artifactContractSpec().Nodes[1]
			if _, err := freezeCommandInputs(context.Background(), t.TempDir(), consumer, deps); err != nil {
				t.Fatalf("unchanged receipt rejected: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "changed":
				err = os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{"ok":false}`), 0600)
			case "missing":
				err = os.Remove(filepath.Join(dir, "result.json"))
			case "symlink":
				if err = os.Remove(filepath.Join(dir, "result.json")); err == nil {
					err = os.Symlink(filepath.Join(dir, "stdout.log"), filepath.Join(dir, "result.json"))
				}
			case "failed producer":
				deps[0].Status = "failed"
			case "missing receipt":
				deps[0].Output.Artifacts = deps[0].Output.Artifacts[:1]
			case "cancelled":
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			consumerDir := t.TempDir()
			if _, err := freezeCommandInputs(ctx, consumerDir, consumer, deps); err == nil {
				t.Fatal("unusable input bytes accepted")
			}
			if entries, err := os.ReadDir(consumerDir); err != nil || len(entries) != 0 {
				t.Fatalf("rejected input left partial copies: %+v %v", entries, err)
			}
		})
	}
}

func TestArtifactContractFreezesOnlyDeclaredInputsWithoutChangingReceipts(t *testing.T) {
	producer, consumerDir := t.TempDir(), t.TempDir()
	for name, content := range map[string]string{"stdout.log": "diagnostic", "result.json": `{"ok":true}`, "unused.json": "not an input"} {
		if err := os.WriteFile(filepath.Join(producer, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := commandNodeOutput(context.Background(), producer, []string{"result.json", "unused.json"})
	if err != nil {
		t.Fatal(err)
	}
	deps := []workergraph.NodeState{{ID: "source", Status: "succeeded", Output: out}}
	before, _ := json.Marshal(deps)
	views, err := freezeCommandInputs(context.Background(), consumerDir, artifactContractSpec().Nodes[1], deps)
	if err != nil || len(views) != 1 || len(views[0].Output.Files) != 1 {
		t.Fatalf("input declaration was not honored: %+v %v", views, err)
	}
	frozen := views[0].Output.Files["result.json"]
	if !strings.HasPrefix(frozen.Path, consumerDir+string(filepath.Separator)) || frozen.Path == filepath.Join(producer, "result.json") || frozen.SHA256 != out.Artifacts[1].SHA256 {
		t.Fatalf("input still points at producer data: %+v", frozen)
	}
	info, err := os.Stat(frozen.Path)
	if err != nil || info.Mode().Perm()&0222 != 0 {
		t.Fatalf("frozen input is not read-only: %v %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(producer, "result.json"), []byte(`{"ok":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(frozen.Path)
	if err != nil || string(content) != `{"ok":true}` {
		t.Fatalf("producer mutation changed frozen bytes: %q %v", content, err)
	}
	after, _ := json.Marshal(deps)
	if string(before) != string(after) || !reflect.DeepEqual(views[0].Output.Output, deps[0].Output) {
		t.Fatal("freezing rewrote durable producer receipts")
	}
	files := 0
	err = filepath.WalkDir(consumerDir, func(_ string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			files++
		}
		return err
	})
	if err != nil || files != 1 {
		t.Fatalf("undeclared artifact or log was copied: files=%d err=%v", files, err)
	}
	link := filepath.Join(t.TempDir(), "redirected")
	if err := os.Symlink(consumerDir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := freezeCommandInputs(context.Background(), link, artifactContractSpec().Nodes[1], deps); err == nil || !strings.Contains(err.Error(), "consumer input directory") {
		t.Fatalf("input copy did not reject the redirected consumer directory: %v", err)
	}
}
