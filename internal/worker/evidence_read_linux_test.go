//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func retainedReadFixture(t *testing.T, workspace, run, original string) board.EvidenceRef {
	t.Helper()
	source := filepath.Join(workspace, run+".txt")
	if err := os.WriteFile(source, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(workspace, ".pwnmesh", "runs", run)
	payload, _ := json.Marshal(map[string]any{"evidence": []board.EvidenceRef{{Path: source, StartLine: 1, EndLine: 1}}})
	action, err := prepareEvidence(context.Background(), Job{Kind: "explore", Workspace: workspace, RunID: run}, runDir, board.StateAction{Op: "fact", IdempotencyKey: "frozen", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	var prepared struct {
		Evidence []board.EvidenceRef `json:"evidence"`
	}
	if err = json.Unmarshal(action.Payload, &prepared); err != nil || len(prepared.Evidence) != 1 {
		t.Fatalf("invalid frozen fixture: %s %v", action.Payload, err)
	}
	// Later control runs must not depend on a producer still running or its
	// original workspace file remaining available.
	if err = os.Remove(source); err != nil {
		t.Fatal(err)
	}
	return prepared.Evidence[0]
}

func retainedReadTool(t *testing.T, workspace string, state *board.State) agent.Tool {
	t.Helper()
	job := Job{Kind: "reason", RunID: "reader", Workspace: workspace, Graph: state.Graph, State: state}
	options := Options{}
	if err := ConfigureRuntimeTools(job, &options); err != nil {
		t.Fatal(err)
	}
	return snapshotRuntimeTool(t, options, "read_evidence")
}

func TestReadEvidenceLegacyDirectory(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprintf("symlink=%t", symlink), func(t *testing.T) {
			workspace := t.TempDir()
			original := "selected\nlegacy full original evidence\n"
			ref := retainedReadFixture(t, workspace, "producer", original)
			current := filepath.Join(workspace, ".pwnmesh")
			legacy := filepath.Join(workspace, ".xloom")
			var err error
			if symlink {
				err = os.Symlink(current, legacy)
			} else {
				err = os.Rename(current, legacy)
			}
			if err != nil {
				t.Fatal(err)
			}
			ref.Path = filepath.Join(legacy, "runs", ref.RunID, "evidence", filepath.Base(ref.Path))
			state := board.State{FactRecords: []board.FactRecord{{ID: "fact", Evidence: []board.EvidenceRef{ref}}}}
			tool := retainedReadTool(t, workspace, &state)
			raw, err := tool.Execute(context.Background(), json.RawMessage(`{"id":"fact","evidence_index":0}`))
			if symlink {
				if err == nil || raw != "" {
					t.Fatalf("legacy directory symlink exposed evidence: %s %v", raw, err)
				}
				return
			}
			var page evidenceContentPage
			if err != nil || json.Unmarshal([]byte(raw), &page) != nil || page.Content != original || page.SHA256 != graphRecordVersion([]byte(original)) {
				t.Fatalf("legacy evidence unreadable: %s %v", raw, err)
			}
		})
	}
}

func TestReadEvidenceRecoversFullOriginalAcrossPagesAndOwnerTypes(t *testing.T) {
	workspace := t.TempDir()
	original := "selected opening line\n" + strings.Repeat("unchosen 数据\n", 90) + "decisive contrary evidence in the middle\n" + strings.Repeat("trailing 🧵 material\n", 80)
	ref := retainedReadFixture(t, workspace, "producer", original)
	state := board.State{
		FactRecords: []board.FactRecord{{ID: "fact", Evidence: []board.EvidenceRef{ref, ref}}},
		Findings:    []board.Finding{{ID: "finding", Evidence: []board.EvidenceRef{ref, ref}}},
		Candidates:  []board.Candidate{{ID: "candidate", Evidence: []board.EvidenceRef{ref, ref}}},
	}
	tool := retainedReadTool(t, workspace, &state)
	wantSHA256 := graphRecordVersion([]byte(original))
	for _, id := range []string{"fact", "finding", "candidate"} {
		t.Run(id, func(t *testing.T) {
			index, offset, version, recovered, pages := 1, 0, "", "", 0
			for {
				input, _ := json.Marshal(evidenceReadRequest{ID: id, EvidenceIndex: &index, ByteOffset: offset, Limit: 37, EvidenceVersion: version})
				raw, err := tool.Execute(context.Background(), input)
				if err != nil {
					t.Fatal(err)
				}
				var page evidenceContentPage
				if json.Unmarshal([]byte(raw), &page) != nil || page.ID != id || page.EvidenceIndex != index || page.ByteOffset != offset || page.TotalBytes != len(original) || page.SHA256 != wantSHA256 || !utf8.ValidString(page.Content) || len(page.Content) > 37 || page.StateVersion != board.DecisionStateVersion(state) {
					t.Fatalf("invalid original page: %s", raw)
				}
				if version != "" && version != page.EvidenceVersion {
					t.Fatal("continuation changed evidence identity")
				}
				version, recovered, pages = page.EvidenceVersion, recovered+page.Content, pages+1
				if page.NextByteOffset == nil {
					break
				}
				if *page.NextByteOffset <= offset {
					t.Fatal("continuation made no progress")
				}
				offset = *page.NextByteOffset
			}
			if pages < 2 || recovered != original || !strings.Contains(recovered, "contrary evidence") || strings.Contains(ref.Excerpt, "contrary evidence") {
				t.Fatal("full original was replaced by the selected excerpt")
			}
		})
	}
}

func TestReadEvidenceRejectsUnboundInputAndChangedReferences(t *testing.T) {
	workspace := t.TempDir()
	ref := retainedReadFixture(t, workspace, "producer", "selected\nold full original\n")
	state := board.State{FactRecords: []board.FactRecord{{ID: "fact", Evidence: []board.EvidenceRef{ref}}}}
	tool := retainedReadTool(t, workspace, &state)
	for _, input := range []string{
		`{"id":"fact","evidence_index":0,"path":"/etc/passwd"}`,
		`{"id":"unknown","evidence_index":0}`,
		`{"id":"fact"}`,
		`{"id":"fact","evidence_index":null}`,
		`{"id":"fact","evidence_index":-1}`,
		`{"id":"fact","evidence_index":1}`,
		`{"id":"fact","evidence_index":0,"byte_offset":1}`,
		`{"id":"fact","evidence_index":0,"limit":3}`,
		`{"id":"fact","evidence_index":0,"limit":16385}`,
		`{"id":"fact","evidence_index":0,"evidence_version":"forged"}`,
		`{"id":"fact","evidence_index":0} {}`,
	} {
		if output, err := tool.Execute(context.Background(), json.RawMessage(input)); err == nil || output != "" {
			t.Fatalf("invalid read returned raw bytes: %s %s %v", input, output, err)
		}
	}
	raw, err := tool.Execute(context.Background(), json.RawMessage(`{"id":"fact","evidence_index":0,"limit":4}`))
	if err != nil {
		t.Fatal(err)
	}
	var first evidenceContentPage
	if err := json.Unmarshal([]byte(raw), &first); err != nil {
		t.Fatal(err)
	}
	state.FactRecords[0].Evidence[0] = retainedReadFixture(t, workspace, "replacement", "selected\nnew full original\n")
	input := fmt.Sprintf(`{"id":"fact","evidence_index":0,"byte_offset":4,"evidence_version":%q}`, first.EvidenceVersion)
	if output, err := tool.Execute(context.Background(), json.RawMessage(input)); err == nil || !strings.Contains(err.Error(), "evidence_changed") || output != "" {
		t.Fatalf("rebound continuation read a different original: %s %v", output, err)
	}
}

func TestReadEvidenceVerifiesWholeHashExcerptAndScope(t *testing.T) {
	for _, scenario := range []string{"middle_tamper", "wrong_excerpt", "other_workspace", "wrong_run", "file_symlink", "directory_symlink", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			workspace := t.TempDir()
			ref := retainedReadFixture(t, workspace, "producer", "selected\nfull original evidence\n")
			switch scenario {
			case "middle_tamper":
				if err := os.Chmod(ref.Path, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(ref.Path, []byte("selected\nFAKE original evidence\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong_excerpt":
				ref.Excerpt = "invented"
			case "other_workspace":
				ref = retainedReadFixture(t, t.TempDir(), "producer", "selected\nother project secret\n")
			case "wrong_run":
				ref.RunID = "another_run"
			case "file_symlink":
				other := retainedReadFixture(t, t.TempDir(), "producer", "selected\nfull original evidence\n")
				if err := os.Remove(ref.Path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other.Path, ref.Path); err != nil {
					t.Fatal(err)
				}
			case "directory_symlink":
				dir := filepath.Dir(ref.Path)
				if err := os.Rename(dir, dir+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir+"-saved", dir); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(ref.Path); err != nil {
					t.Fatal(err)
				}
			}
			state := board.State{FactRecords: []board.FactRecord{{ID: "fact", Evidence: []board.EvidenceRef{ref}}}}
			if output, err := retainedReadTool(t, workspace, &state).Execute(context.Background(), json.RawMessage(`{"id":"fact","evidence_index":0}`)); err == nil || output != "" {
				t.Fatalf("invalid retained file exposed bytes: %s %v", output, err)
			}
		})
	}
}

func TestCuratorReadEvidenceUsesFrozenSnapshotBridge(t *testing.T) {
	j, runDir := curationJob(t), t.TempDir()
	j.Workspace = t.TempDir()
	original := "selected\ncontrary evidence outside selected line\n"
	ref := retainedReadFixture(t, j.Workspace, "finished_producer", original)
	j.State.FactRecords = []board.FactRecord{{ID: "fact", Evidence: []board.EvidenceRef{ref}}}
	frozen := *j.State
	raw, _ := json.Marshal(frozen)
	j.InputSnapshot = &board.InputSnapshot{Version: 1, ID: graphRecordVersion(raw), ProjectID: j.Graph.Project.ID, Generation: j.Graph.Project.Generation, Revision: frozen.Revision, StateVersion: board.DecisionStateVersion(frozen)}
	var err error
	j.InputView, err = board.CurationContextView(frozen, board.DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	j.State, j.Graph = nil, board.Graph{Project: j.Graph.Project}
	reads := 0
	bridge := &draftTestBridge{dir: runDir, handle: func(r GraphRequest) (any, error) {
		reads++
		if r.Op != "read_snapshot" || r.Section != "evidence" || r.ExpectedVersion != j.InputSnapshot.StateVersion || r.Limit != 1 || len(r.IDs) != 1 || r.IDs[0] != "fact" {
			t.Fatalf("raw lookup escaped curator input: %+v", r)
		}
		return GraphPage(frozen, r)
	}}
	options := Options{RunDir: runDir, Output: bridge}
	if err := ConfigureRuntimeTools(j, &options); err != nil {
		t.Fatal(err)
	}
	for _, tool := range options.Tools {
		if tool.Name != "read_graph" && tool.Name != "graph_action" && tool.Name != "read_evidence" {
			t.Fatalf("curator gained arbitrary execution capability: %s", tool.Name)
		}
	}
	result, err := snapshotRuntimeTool(t, options, "read_evidence").Execute(context.Background(), json.RawMessage(`{"id":"fact","evidence_index":0}`))
	var page evidenceContentPage
	if err != nil || json.Unmarshal([]byte(result), &page) != nil || page.Content != original || page.StateVersion != j.InputSnapshot.StateVersion || reads != 1 {
		t.Fatalf("snapshot raw read failed: %s %v reads=%d", result, err, reads)
	}
}

func TestReadEvidenceBoundsEscapedContentAndUTF8Offsets(t *testing.T) {
	workspace := t.TempDir()
	original := "selected\n" + strings.Repeat("\x00\"\\🧵", 5000)
	ref := retainedReadFixture(t, workspace, "producer", original)
	state := board.State{FactRecords: []board.FactRecord{{ID: "fact", Evidence: []board.EvidenceRef{ref}}}}
	tool := retainedReadTool(t, workspace, &state)
	raw, err := tool.Execute(context.Background(), json.RawMessage(`{"id":"fact","evidence_index":0}`))
	if err != nil || len(raw) >= MaxGraphRPCBytes {
		t.Fatalf("raw page exceeded transport budget: %d %v", len(raw), err)
	}
	var first evidenceContentPage
	if err := json.Unmarshal([]byte(raw), &first); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{len("selected\n") + 4, len(original) + 1} {
		input := fmt.Sprintf(`{"id":"fact","evidence_index":0,"byte_offset":%d,"evidence_version":%q}`, offset, first.EvidenceVersion)
		if output, err := tool.Execute(context.Background(), json.RawMessage(input)); err == nil || output != "" {
			t.Fatalf("invalid UTF-8/outside offset accepted: %s %v", output, err)
		}
	}
	if !readOnlyObservation("read_evidence") {
		t.Fatal("raw evidence reads bypass repeated observation checks")
	}
	metrics := newDecisionMetrics()
	metrics.observe(agent.Event{Type: "tool_start", ToolName: "read_evidence"})
	metrics.observe(agent.Event{Type: "tool_end", ToolName: "read_evidence", Error: "unavailable"})
	if metrics.GraphReads != 1 || metrics.GraphReadFailures != 1 {
		t.Fatal("raw evidence reads disappeared from decision metrics")
	}
}

func TestReadEvidenceRecoversEscapedReferenceFromBoundRecordPages(t *testing.T) {
	workspace := t.TempDir()
	original := strings.Repeat("<", 8191) + "\npreviously unselected original bytes\n"
	ref := retainedReadFixture(t, workspace, "producer", original)
	state := board.State{FactRecords: []board.FactRecord{{ID: "fact", Evidence: []board.EvidenceRef{ref}}}}
	version := board.DecisionStateVersion(state)
	encoded, _ := json.Marshal(ref)
	if len(encoded) <= (maxGraphPageBytes-1024)/6 {
		t.Fatal("fixture does not require multiple bounded record fragments")
	}
	for _, scenario := range []string{"complete", "changed_record", "changed_version"} {
		t.Run(scenario, func(t *testing.T) {
			reads := 0
			tool := rawEvidenceTool(Job{Kind: "reason", Workspace: workspace}, &Options{}, func(_ context.Context, r GraphRequest) (string, error) {
				reads++
				var result any
				if r.ByteOffset == nil {
					// Exercise the existing graph protocol's lossless record path,
					// even if this valid ref fits today's outer page byte budget.
					marker, _ := json.Marshal(graphRecordReference{true, 0, len(encoded), version, graphRecordVersion(encoded), graphRecordReadMore})
					result = graphPage{Section: "evidence", StateVersion: version, Total: 1, Items: []json.RawMessage{marker}}
				} else {
					if r.ExpectedVersion != version || r.RecordVersion != graphRecordVersion(encoded) || r.Offset != 0 || len(r.IDs) != 1 || r.IDs[0] != "fact" {
						t.Fatalf("record read lost reference binding: %+v", r)
					}
					part, err := graphRecordContent(r, version, encoded)
					if err != nil {
						return "", err
					}
					if reads > 2 && scenario == "changed_record" {
						part.RecordVersion = strings.Repeat("f", 64)
					}
					if reads > 2 && scenario == "changed_version" {
						part.StateVersion = strings.Repeat("f", 64)
					}
					result = part
				}
				raw, err := json.Marshal(result)
				return string(raw), err
			})
			output, err := tool.Execute(context.Background(), json.RawMessage(`{"id":"fact","evidence_index":0}`))
			if scenario != "complete" {
				if err == nil || output != "" {
					t.Fatalf("changed record returned evidence: %s %v", output, err)
				}
				return
			}
			var page evidenceContentPage
			if err != nil || json.Unmarshal([]byte(output), &page) != nil || page.Content != original || reads < 3 || len(output) >= MaxGraphRPCBytes {
				t.Fatalf("escaped reference was not recovered completely: len=%d err=%v reads=%d", len(output), err, reads)
			}
		})
	}
}

func TestFailedReadEvidenceDoesNotSatisfyDecisionRecoveryReread(t *testing.T) {
	j, runDir := draftRunJob(t), t.TempDir()
	j.Workspace = t.TempDir()
	original := "selected\noriginal retained context\n"
	ref := retainedReadFixture(t, j.Workspace, "producer", original)
	j.State.FactRecords = []board.FactRecord{{ID: "fact", Evidence: []board.EvidenceRef{ref}}}
	j.Decision.StateVersion = board.DecisionStateVersion(*j.State)
	bridge := &draftTestBridge{dir: runDir, handle: func(r GraphRequest) (any, error) {
		if r.Op != "read_graph" || r.ExpectedVersion != j.Decision.StateVersion {
			t.Fatalf("evidence lookup escaped the current decision fence: %+v", r)
		}
		return GraphPage(*j.State, r)
	}}
	options := Options{RunDir: runDir, Output: bridge}
	if err := ConfigureRuntimeTools(j, &options); err != nil {
		t.Fatal(err)
	}
	options.decision.reread, options.decision.overviewRead = true, true
	if err := os.Remove(ref.Path); err != nil {
		t.Fatal(err)
	}
	tool := snapshotRuntimeTool(t, options, "read_evidence")
	input := json.RawMessage(`{"id":"fact","evidence_index":0}`)
	if output, err := tool.Execute(context.Background(), input); err == nil || output != "" || !options.decision.reread {
		t.Fatalf("hidden reference lookup satisfied recovery without evidence: %s %v", output, err)
	}
	if err := os.WriteFile(ref.Path, []byte(original), 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), input); err != nil || !options.decision.reread {
		t.Fatalf("successful raw read bypassed the next model request: %v", err)
	}
	loop := &agent.Loop{}
	options.decision.beforeRequest(loop)
	if options.decision.reread || len(loop.ContextData) != 1 || !strings.Contains(loop.ContextData[0], "original retained context") {
		t.Fatal("successful raw observation was not retained for the model before recovery")
	}
}
