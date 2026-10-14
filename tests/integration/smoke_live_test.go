//go:build linux

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

const smokeValues = "2\n3\n5\n"
const smokeValuesPath = "/workspace/smoke-values.txt"
const smokeReceiptPath = "/workspace/smoke-receipt.json"

func validateSmokeDelivery(state board.State, files map[string][]byte) []string {
	failures := []string{}
	check := func(ok bool, message string) {
		if !ok {
			failures = append(failures, message)
		}
	}
	check(state.Graph.Project.Status == "completed" && state.Graph.Project.OrchestrationVersion == 1, "smoke project did not complete in orchestration mode")
	check(bytes.Equal(files[smokeValuesPath], []byte(smokeValues)), "original smoke input bytes changed or disappeared")
	var receipt struct {
		Sum    int    `json:"sum"`
		SHA256 string `json:"dataset_sha256"`
	}
	check(json.Unmarshal(files[smokeReceiptPath], &receipt) == nil && receipt.Sum == 2+3+5 && receipt.SHA256 == fmt.Sprintf("%x", sha256.Sum256([]byte(smokeValues))), "receipt does not match the independently calculated sum and input hash")
	var steps []board.Step
	for _, step := range state.Steps {
		if !staleRepairCompletionMarker(state, step) {
			steps = append(steps, step)
		}
	}
	check(len(steps) == 1, "expected exactly one execution Step")
	check(len(state.Candidates) == 0 && len(state.Disputes) == 0, "unexpected candidate or dispute work")
	if len(steps) != 1 || steps[0].Result == nil || steps[0].Worker == nil {
		check(false, "execution Step has no result or Worker")
		return failures
	}
	step := steps[0]
	facts := map[string]board.FactRecord{}
	produced := 0
	for _, fact := range state.FactRecords {
		facts[fact.ID] = fact
		if fact.SourceStepID == step.ID {
			produced++
		}
	}
	fact := facts[*step.Result]
	_, succeeded := orchestrationSuccessfulRun(state.Graph.Project, step, *step.Worker, facts, files)
	check(succeeded && produced == 1 && fact.Scope == "smoke-v1", "Worker did not finish successfully with its single smoke fact")
	inputEvidence, receiptEvidence := false, false
	for _, ref := range fact.Evidence {
		check(orchestrationEvidenceValid(ref, fact.RunID, files), "fact evidence lost its original bytes or producer identity")
		inputEvidence = inputEvidence || bytes.Equal(files[ref.Path], []byte(smokeValues))
		receiptEvidence = receiptEvidence || bytes.Equal(files[ref.Path], files[smokeReceiptPath])
	}
	check(inputEvidence && receiptEvidence, "fact does not retain both input and calculation receipt")
	check(orchestrationCommandOutput(fact.RunID, files[smokeReceiptPath], files), "receipt was not printed by a successful Worker bash command")
	supported := false
	for _, goal := range state.Goals {
		if goal.ID == "goal" {
			supported = goal.Status == "achieved" && goal.SupportValid && slices.Contains(goal.Sources, fact.ID)
		}
	}
	check(supported, "root goal does not cite the successful Worker fact")
	return failures
}

func smokeDeliveryFixture(t *testing.T) (board.State, map[string][]byte) {
	t.Helper()
	run, workerID, factID := "smoke-run", "live@smoke-run", "smoke-fact"
	state := board.State{Graph: board.Graph{Project: board.Project{ID: "smoke-project", Status: "completed", OrchestrationVersion: 1}}}
	files := map[string][]byte{
		smokeValuesPath:  []byte(smokeValues),
		smokeReceiptPath: []byte(fmt.Sprintf("{\"sum\":10,\"dataset_sha256\":\"%x\"}\n", sha256.Sum256([]byte(smokeValues)))),
	}
	fact := board.FactRecord{ID: factID, Description: "Local values sum to 10", Scope: "smoke-v1", ObservedAt: "2026-10-15T00:00:00Z", Status: "valid", RunID: workerID, SourceStepID: "calculate",
		Evidence: []board.EvidenceRef{retainOrchestrationEvidence(run, files[smokeValuesPath], files), retainOrchestrationEvidence(run, files[smokeReceiptPath], files)}}
	state.FactRecords = []board.FactRecord{fact}
	state.Steps = []board.Step{{ID: fact.SourceStepID, Status: "completed", SupportValid: true, Worker: &workerID, Result: &factID}}
	state.Goals = []board.Goal{{ID: "goal", Status: "achieved", SupportValid: true, Sources: []string{factID}}}
	base := "/workspace/.pwnmesh/runs/" + run + "/"
	files[base+"job.json"], _ = json.Marshal(worker.Job{RunID: run, Kind: "explore", GraphRPC: true, ResultContractVersion: 2, Graph: state.Graph, Intent: &board.Intent{ID: fact.SourceStepID}})
	files[base+"session.json"], _ = json.Marshal(map[string]any{"run_id": run, "identity": map[string]string{"project_id": state.Graph.Project.ID, "step_id": fact.SourceStepID, "run_id": run},
		"result": worker.Result{Status: "success", Text: fmt.Sprintf(`{"accepted":true,"outcome":"completed","data":{"fact_id":%q}}`, factID)}})
	output, _ := json.Marshal(string(files[smokeReceiptPath]))
	for _, message := range []agent.Message{
		{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "calculate", Name: "bash", Input: json.RawMessage(`{"command":"sum actual input"}`)}}},
		{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "calculate", Content: output}}},
	} {
		raw, _ := json.Marshal(agent.Event{Type: "message", Message: &message})
		files[base+"events.jsonl"] = append(files[base+"events.jsonl"], append(raw, '\n')...)
	}
	return state, files
}

func TestSmokeDeliveryRequiresRetainedProof(t *testing.T) {
	state, files := smokeDeliveryFixture(t)
	if failures := validateSmokeDelivery(state, files); len(failures) != 0 {
		t.Fatal(failures)
	}
	for name, damage := range map[string]func(*board.State, map[string][]byte){
		"completion only": func(s *board.State, files map[string][]byte) { s.Steps = nil },
		"changed input":   func(s *board.State, files map[string][]byte) { files[smokeValuesPath] = []byte("2\n3\n6\n") },
		"wrong sum":       func(s *board.State, files map[string][]byte) { files[smokeReceiptPath] = []byte(`{"sum":11}`) },
		"missing evidence": func(s *board.State, files map[string][]byte) {
			delete(files, s.FactRecords[0].Evidence[0].Path)
		},
		"missing command": func(s *board.State, files map[string][]byte) {
			delete(files, "/workspace/.pwnmesh/runs/smoke-run/events.jsonl")
		},
		"missing session": func(s *board.State, files map[string][]byte) {
			delete(files, "/workspace/.pwnmesh/runs/smoke-run/session.json")
		},
		"extra step":   func(s *board.State, files map[string][]byte) { s.Steps = append(s.Steps, s.Steps[0]) },
		"invalid fact": func(s *board.State, files map[string][]byte) { s.FactRecords[0].SupportInvalid = true },
		"unsupported goal": func(s *board.State, files map[string][]byte) {
			s.Goals[0].Sources = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			state, files := smokeDeliveryFixture(t)
			damage(&state, files)
			if len(validateSmokeDelivery(state, files)) == 0 {
				t.Fatal("accepted broken smoke evidence")
			}
		})
	}
}
