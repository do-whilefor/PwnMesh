package protocol_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/board/model"
)

// Freeze the additional wire boundaries moved out of Board. Aliases must keep
// assignment compatibility, null/empty distinctions and private field omission.
func TestExtractedExecutionValuesPreserveWireBytes(t *testing.T) {
	var input board.InputFile = model.InputFile{ID: "input", Name: "proof.txt", Size: 4, SHA256: "hash", Path: "/workspace/proof.txt", CreatedAt: "created"}
	var execution board.Execution = model.Execution{ProjectID: "project", ID: "run", Kind: "explore", Intent: "step", Lease: "backend@run", Job: json.RawMessage(`{"input":true}`), Status: "running", Result: json.RawMessage(`{"status":"success"}`)}
	var batch board.DecisionBatch = model.DecisionBatch{ExpectedVersion: "state", Actions: []model.DecisionAction{{Op: "step", Ref: "next", GapID: "gap", Payload: json.RawMessage(`{"action":"add"}`)}}, Assessment: &model.RootAssessment{Status: "missing", From: []string{}, Description: "Retain scope", Gaps: []model.RequirementGap{{ID: "gap", InputIDs: []string{"origin"}, Description: "Inspect evidence"}}}}
	var receipt board.DecisionReceipt = model.DecisionReceipt{Committed: true, StateVersion: "state", Results: []model.StateActionResult{}, IDs: map[string]string{}, CompletionReview: &model.CompletionReview{From: []string{}, FactRecords: []model.FactRecord{}}}
	var step board.ScheduleStep = model.ScheduleStep{ID: "step", Status: "open", Ready: true, WritePaths: []string{"private-admission-path"}}
	var updates board.ExecuteUpdates = model.ExecuteUpdates{Version: 1, ExecuteUpdateCursor: model.ExecuteUpdateCursor{ProjectID: "project", Generation: 2, StepID: "step", RunID: "run", Revision: 3}, FromRevision: 3, ToRevision: 4, StateVersion: "state", Complete: true, InvalidSources: []string{}, Facts: []model.FactRecord{}, Relations: []model.FactRelation{}, Omitted: map[string]int{}}
	var trace board.TraceRun = model.TraceRun{ProjectID: "project", Generation: 2, RunID: "run", StepID: "step", Status: "running", Workspace: "/workspace", JobSHA256: "hash", RegisteredJob: json.RawMessage(`{"private":true}`)}
	for _, test := range []struct {
		name  string
		value any
		want  string
	}{
		{"input", input, `{"id":"input","name":"proof.txt","size":4,"sha256":"hash","path":"/workspace/proof.txt","created_at":"created"}`},
		{"execution", execution, `{"project_id":"project","id":"run","namespace":"","backend":"","kind":"explore","intent":"step","lease":"backend@run","job":{"input":true},"retry_key":"","status":"running","result":{"status":"success"},"resumes":0,"created_at":"","updated_at":""}`},
		{"batch", batch, `{"expected_version":"state","actions":[{"op":"step","ref":"next","gap_id":"gap","payload":{"action":"add"}}],"assessment":{"status":"missing","from":[],"description":"Retain scope","gaps":[{"id":"gap","input_ids":["origin"],"description":"Inspect evidence"}]}}`},
		{"receipt", receipt, `{"committed":true,"state_version":"state","results":[],"ids":{},"completed":false,"completion_review":{"state_version":"","acceptance":"","user_inputs":null,"hints":null,"from":[],"description":"","fact_records":[]}}`},
		{"step", step, `{"id":"step","status":"open","priority":0,"created_at":"","ready":true}`},
		{"updates", updates, `{"version":1,"project_id":"project","generation":2,"step_id":"step","run_id":"run","revision":3,"from_revision":3,"to_revision":4,"state_version":"state","complete":true,"invalid_sources":[],"facts":[],"relations":[],"omitted":{}}`},
		{"trace", trace, `{"project_id":"project","generation":2,"run_id":"run","step_id":"step","status":"running","workspace":"/workspace","job_sha256":"hash"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(test.value)
			if err != nil || string(raw) != test.want {
				t.Fatalf("wire boundary changed: %s, %v; want %s", raw, err, test.want)
			}
			restored := reflect.New(reflect.TypeOf(test.value))
			if err := json.Unmarshal(raw, restored.Interface()); err != nil {
				t.Fatal(err)
			}
			replayed, err := json.Marshal(restored.Interface())
			if err != nil || !bytes.Equal(raw, replayed) {
				t.Fatalf("retained input changed on replay: %s, %v", replayed, err)
			}
		})
	}
}
