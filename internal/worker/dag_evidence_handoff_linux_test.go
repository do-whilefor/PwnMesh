//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/worker/dag"
	"pwnmesh/internal/workergraph"
)

func TestDAGEvidenceHandoffRejectsChangedVerifiedOutput(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	j.ResultContractVersion = 2
	o := Options{RunDir: dir}
	if err := ConfigureRuntimeTools(j, &o); err != nil {
		t.Fatal(err)
	}
	raw, err := marshalCommandGraphCall(dag.Spec{Key: "handoff", Nodes: []dag.Node{{
		ID: "observe", Command: `printf 'original observation\n'`, Resources: []string{},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := commandGraphTool(j, o).Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Evidence []dag.Evidence `json:"verified_evidence"`
	}
	if err := json.Unmarshal([]byte(reply), &receipt); err != nil || len(receipt.Evidence) != 1 {
		t.Fatalf("missing verified observation: %+v %v", receipt, err)
	}
	path := receipt.Evidence[0].Path
	if err := os.WriteFile(path, []byte("replacement never observed by parent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := o.stepFinish.tool().Execute(context.Background(), finishStepInput(t, path)); err == nil {
		t.Fatal("finish_step accepted changed DAG evidence without binding it to the verified observation")
	}
}

func TestDAGEvidenceHandoffArtifactBinding(t *testing.T) {
	for _, kind := range []string{"command", "agent"} {
		for _, changed := range []bool{false, true} {
			name := kind + "/unchanged"
			if changed {
				name = kind + "/changed"
			}
			t.Run(name, func(t *testing.T) {
				j, dir := graphWrapperJob(t), t.TempDir()
				j.ResultContractVersion = 2
				o := Options{RunDir: dir}
				command := `mkdir -p reports; printf 'original\n' > reports/result.txt; printf 'observed\n'`
				node := dag.Node{ID: "observe", Kind: kind, Resources: []string{}, Artifacts: []string{"reports/result.txt"}}
				if kind == "command" {
					node.Command = command
				} else {
					node.Task = "Create the declared result file."
					o.graphProvider = func(string) (agent.Provider, error) {
						turn := 0
						return scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
							turn++
							if turn == 1 {
								args, _ := json.Marshal(map[string]string{"command": command})
								return draftModelCall("write-result", "bash", string(args)), nil
							}
							return agent.Text("assistant", "Created the declared observation."), nil
						}), nil
					}
				}
				raw, _ := marshalCommandGraphCall(dag.Spec{Key: "artifact", Nodes: []dag.Node{node}})
				if _, err := commandGraphTool(j, o).Execute(context.Background(), raw); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "graph-tools", "artifact", "nodes", "observe", "reports", "result.txt")
				if changed {
					if err := os.WriteFile(path, []byte("unobserved replacement\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				prepared, err := prepareFinalEvidence(context.Background(), j, dir, Result{Status: "success", Text: finalEvidenceOutput(path)}, nil)
				if changed {
					if err == nil || !strings.Contains(err.Error(), "SHA-256 changed") {
						t.Fatalf("changed accepted artifact was not rejected: %v", err)
					}
				} else if err != nil || finalEvidenceRefs(t, j, prepared)[0].Excerpt != "original\n" {
					t.Fatalf("unchanged artifact was not retained: %v", err)
				}
			})
		}
	}
}

func TestDAGEvidenceHandoffValidatesSnapshotBindings(t *testing.T) {
	for _, mutation := range []string{"run", "definition", "unaccepted"} {
		t.Run(mutation, func(t *testing.T) {
			j, dir, checkpoint := reuseSource(t, false)
			node, _ := commandNodeValue(t, checkpoint, "download")
			path := filepath.Join(dir, "graph-tools", "original", "nodes", "download", "result.json")
			switch mutation {
			case "run":
				checkpoint.RunID = "other-run"
			case "definition":
				definition := filepath.Join(dir, "graph-tools", "original", "definitions", node.DefinitionSHA256+".json")
				if err := os.WriteFile(definition, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "unaccepted":
				checkpoint.Nodes[0].Status = "failed"
			}
			raw, _ := json.Marshal(checkpoint)
			if err := os.WriteFile(filepath.Join(dir, "graph-tools", "original", "graph.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := dag.VerifyEvidence(context.Background(), commandGraphScope(j, dir), path, content); err == nil {
				t.Fatal("invalid DAG receipt authorized shared evidence")
			}
		})
	}
}

func TestDAGEvidenceHandoffSupportsRepairGraphsAndOriginalReusePaths(t *testing.T) {
	j, dir, _ := reuseSource(t, false)
	spec := dag.Spec{Key: "repair", Nodes: []dag.Node{
		{ID: "imported", Kind: "reuse", ReuseFrom: &dag.Reuse{Key: "original", Node: "download"}, Resources: []string{}},
		{ID: "new", Command: "printf 'fresh observation\n'", Resources: []string{}, DependsOn: []workergraph.Dependency{{ID: "imported"}}},
	}}
	checkpoint, err := commandGraphCall(t, context.Background(), j, dir, spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"imported", "new"} {
		_, output := commandNodeValue(t, checkpoint, id)
		raw, err := os.ReadFile(output.OutputPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := dag.VerifyEvidence(context.Background(), commandGraphScope(j, dir), output.OutputPath, raw); err != nil {
			t.Fatalf("valid repair evidence rejected: %v", err)
		}
		if err := dag.VerifyEvidence(context.Background(), commandGraphScope(j, dir), output.OutputPath, []byte("replacement")); err == nil {
			t.Fatal("changed repaired or reused evidence accepted")
		}
	}
}

func TestDAGEvidenceHandoffPreservesFreshFilesAndFailureDiagnostics(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	spec := dag.Spec{Key: "diagnostics", Nodes: []dag.Node{{ID: "failed", Command: "printf failure; printf partial > result.txt; exit 3", Resources: []string{}, Artifacts: []string{"result.txt"}}}}
	if _, err := commandGraphCall(t, context.Background(), j, dir, spec); err == nil {
		t.Fatal("failure fixture unexpectedly succeeded")
	}
	for _, name := range []string{"stdout.log", "fresh.txt", "result.txt"} {
		path := filepath.Join(dir, "graph-tools", "diagnostics", "nodes", "failed", name)
		if name == "fresh.txt" {
			if err := os.WriteFile(path, []byte("fresh observation"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		err = dag.VerifyEvidence(context.Background(), commandGraphScope(j, dir), path, raw)
		if (err != nil) != (name == "result.txt") {
			t.Fatalf("diagnostics/fresh observation contract changed for %s: %v", name, err)
		}
	}
	// Ordinary selections also work when the caller supplies a relative run root.
	if err := dag.VerifyEvidence(context.Background(), commandGraphScope(j, "relative-run"), filepath.Join(j.Workspace, "fresh.txt"), []byte("fresh")); err != nil {
		t.Fatalf("relative run root rejected an unrelated observation: %v", err)
	}
}
