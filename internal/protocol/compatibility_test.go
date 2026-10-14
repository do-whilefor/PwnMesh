package protocol_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/board"
	"pwnmesh/internal/board/model"
	"pwnmesh/internal/config"
	"pwnmesh/internal/protocol"
)

func TestDecisionSnapshotRequiresCurrentProtocol(t *testing.T) {
	for _, version := range []int{0, 1, 2, 3} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			job := protocol.Job{
				Kind: "reason", Graph: model.Graph{Project: model.Project{ID: "project", Generation: 2}},
				InputSnapshot: &model.InputSnapshot{Version: 1, ID: strings.Repeat("a", 64), ProjectID: "project", Generation: 2, Revision: 3, StateVersion: strings.Repeat("b", 64)},
				Decision:      &model.DecisionContext{Version: version, Generation: 2, ToRevision: 3, StateVersion: strings.Repeat("b", 64), View: json.RawMessage(`{}`)},
			}
			if err := protocol.ValidateSnapshotInput(job); (err == nil) != (version == 2) {
				t.Fatalf("decision version %d validation: %v", version, err)
			}
		})
	}
}

// These bytes were captured from Worker before moving its wire definitions.
// Exact field order matters: persisted session identity hashes typed Job JSON.
func TestWireEncodingPreservesExistingExecutions(t *testing.T) {
	for name, fixture := range wireFixtures() {
		t.Run(name, func(t *testing.T) {
			expected, err := os.ReadFile(filepath.Join("testdata", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			expected = bytes.TrimSuffix(expected, []byte("\n"))
			actual, err := json.Marshal(fixture)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actual, expected) {
				t.Fatalf("wire encoding changed\ngot: %s\nwant: %s", actual, expected)
			}
			restored := reflect.New(reflect.TypeOf(fixture))
			if err := json.Unmarshal(expected, restored.Interface()); err != nil {
				t.Fatal(err)
			}
			replay, err := json.Marshal(restored.Elem().Interface())
			if err != nil || !bytes.Equal(replay, expected) {
				t.Fatalf("retained wire data changed on replay: %s, %v", replay, err)
			}
		})
	}
}

func TestSharedValuesPreserveWireEncoding(t *testing.T) {
	// These values remain assignable through the old package names. Their exact
	// bytes bind frozen inputs and session identity, including nil collections.
	var fact board.FactRecord = model.FactRecord{ID: "fact", Description: "observed", Evidence: []model.EvidenceRef{{RunID: "run", Path: "proof", Excerpt: "line", StartLine: 2, EndLine: 3}}, Status: "valid", RunID: "run", SourceStepID: "step", SupportInvalid: true}
	var step board.Step = model.Step{ID: "step", From: []string{}, GoalID: "goal", Description: "inspect", Status: "open", Priority: 7, DependsOn: []string{"upstream"}, WritePaths: []string{"proof"}, BlockedBy: []string{"upstream"}, SupportValid: true, LatestRunID: "retry"}
	var budget config.Task = model.TaskBudget{Timeout: 10, ConcludeTimeout: 20, MaxIntents: 3, ReasoningEffort: "high"}
	for _, test := range []struct {
		name  string
		value any
		want  string
	}{
		{"fact", fact, `{"id":"fact","description":"observed","scope":"","observed_at":"","evidence":[{"run_id":"run","path":"proof","excerpt":"line","start_line":2,"end_line":3}],"status":"valid","run_id":"run","source_step_id":"step","legacy":false,"support_invalid":true}`},
		{"step", step, `{"id":"step","from":[],"goal_id":"goal","description":"inspect","status":"open","priority":7,"result":null,"worker":null,"created_at":"","depends_on":["upstream"],"write_paths":["proof"],"blocked_by":["upstream"],"support_valid":true,"latest_run_id":"retry"}`},
		{"goal", model.Goal{ID: "goal", ParentID: "root", Sources: []string{}, SupportValid: true}, `{"id":"goal","parent_id":"root","condition":"","status":"","sources":[],"created_at":"","support_valid":true}`},
		{"finding", model.Finding{ID: "finding", Sources: []string{}, CandidateIDs: []string{"candidate"}, DisputeID: "dispute", CuratedRevision: 2}, `{"id":"finding","claim":"","scope":"","status":"","sources":[],"evidence":null,"created_at":"","updated_at":"","support_valid":false,"candidate_ids":["candidate"],"dispute_id":"dispute","curated_revision":2}`},
		{"relation", model.FactRelation{Kind: "supersedes", Source: "new", Target: "old", Reason: "verified", RunID: "run", CreatedAt: "now"}, `{"kind":"supersedes","source":"new","target":"old","reason":"verified","run_id":"run","created_at":"now"}`},
		{"action", model.StateAction{Op: "fact", IdempotencyKey: "key", Payload: json.RawMessage(`{"id":"fact"}`), ExpectedVersion: "version"}, `{"op":"fact","idempotency_key":"key","payload":{"id":"fact"},"expected_version":"version"}`},
		{"receipt", model.StateActionResult{Op: "fact", ID: "fact", Revision: 1, Result: json.RawMessage(`{}`), StateVersion: "version", Unchanged: true, Committed: true, AssetIDs: []string{"asset"}}, `{"op":"fact","id":"fact","revision":1,"result":{},"state_version":"version","unchanged":true,"committed":true,"asset_ids":["asset"]}`},
		{"budget", budget, `{"timeout":10,"conclude_timeout":20,"max_intents":3,"reasoning_effort":"high"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual, err := json.Marshal(test.value)
			if err != nil || string(actual) != test.want {
				t.Fatalf("shared value encoding changed: %s, %v; want %s", actual, err, test.want)
			}
			restored := reflect.New(reflect.TypeOf(test.value))
			if err := json.Unmarshal([]byte(test.want), restored.Interface()); err != nil {
				t.Fatal(err)
			}
			replay, err := json.Marshal(restored.Elem().Interface())
			if err != nil || string(replay) != test.want {
				t.Fatalf("shared value replay changed: %s, %v", replay, err)
			}
		})
	}
}

func TestTraceBindingUsesTypedJobIdentity(t *testing.T) {
	job := wireFixtures()["job"].(protocol.Job)
	job.Workspace = t.TempDir()
	typed, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(typed, &fields); err != nil {
		t.Fatal(err)
	}
	registered, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(registered, typed) {
		t.Fatal("fixture did not reorder the registered job")
	}
	run := board.TraceRun{ProjectID: job.Graph.Project.ID, Generation: job.Graph.Project.Generation, RunID: job.RunID, StepID: job.Intent.ID, Workspace: job.Workspace, RegisteredJob: registered}
	bound, err := protocol.BindTraceRun(run)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(typed)
	if bound.JobSHA256 != hex.EncodeToString(digest[:]) || bound.RegisteredJob != nil {
		t.Fatalf("trace lost typed job identity or retained private job: %+v", bound)
	}
	run.StepID = "foreign"
	if _, err := protocol.BindTraceRun(run); err == nil {
		t.Fatal("trace accepted a different execution identity")
	}
}

func wireFixtures() map[string]any {
	graph := board.Graph{Project: board.Project{ID: "project", Generation: 2, OrchestrationVersion: 1}}
	return map[string]any{
		"job": protocol.Job{
			InputFiles: []board.InputFile{{}}, Repair: &artifactcheck.Spec{}, RunID: "run", PreviousRunID: "previous", GraphRPC: true, ResultContractVersion: 2,
			EnvironmentID: "environment", DecisionRevision: 3, Decision: &board.DecisionContext{Version: 2}, DecisionTrigger: "changed", DecisionTriggers: []string{"changed", "ready"}, DecisionRepeated: true,
			Kind: "explore", WorkerType: "go", Graph: graph,
			State: &board.State{Graph: graph}, InputSnapshot: &board.InputSnapshot{Version: 1}, InputView: json.RawMessage(`{"view":true}`), PreparationKey: "preparation", Intent: &board.Intent{ID: "step"}, DependencyResults: []board.DependencyResult{{}},
			Budget: config.Task{}, Workspace: "/workspace",
		},
		"result":   protocol.Result{RepairCheck: &protocol.RepairCheck{BlankContent: true, Path: "/workspace/proof", ExpectedSHA256: "expected", Outcome: "repaired"}, Type: "result", Text: "done", Conclude: true, Status: "failed", Error: "interrupted", Retryable: true, FailureKind: "transport", FailureCause: "unavailable", StateVersion: "version", Metrics: &protocol.DecisionMetrics{Version: 1}},
		"request":  protocol.GraphRequest{RequestID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Op: "read_graph", Section: "facts", Offset: 2, ByteOffset: new(int), RecordVersion: "record", Limit: 3, ExpectedVersion: "state", IDs: []string{"fact"}, AssetIDs: []string{"asset"}, Action: board.StateAction{Op: "fact"}, Batch: &board.DecisionBatch{}, Updates: &board.ExecuteUpdateCursor{}},
		"response": protocol.GraphResponse{RequestID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Result: json.RawMessage(`{"committed":true}`), Error: "error"},
	}
}
