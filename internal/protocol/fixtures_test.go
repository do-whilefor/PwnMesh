package protocol_test

import (
	"encoding/json"
	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/protocol"
)

func wireFixtures() map[string]any {
	return map[string]any{
		"job": protocol.Job{
			InputFiles: []board.InputFile{{}}, Repair: &artifactcheck.Spec{}, RunID: "run", PreviousRunID: "previous", GraphRPC: true, ResultContractVersion: 2,
			EnvironmentID: "environment", DecisionRevision: 3, Decision: &board.DecisionContext{Version: 2}, DecisionTrigger: "changed", DecisionTriggers: []string{"changed", "ready"}, DecisionRepeated: true,
			Kind: "explore", WorkerType: "go", Graph: board.Graph{Project: board.Project{ID: "project", Generation: 2, OrchestrationVersion: 1}},
			State: &board.State{}, InputSnapshot: &board.InputSnapshot{Version: 1}, InputView: json.RawMessage(`{"view":true}`), PreparationKey: "preparation", Intent: &board.Intent{ID: "step"}, DependencyResults: []board.DependencyResult{{}},
			Budget: config.Task{}, Workspace: "/workspace",
		},
		"result":   protocol.Result{RepairCheck: &protocol.RepairCheck{BlankContent: true, Path: "/workspace/proof", ExpectedSHA256: "expected", Outcome: "repaired"}, Type: "result", Text: "done", Conclude: true, Status: "failed", Error: "interrupted", Retryable: true, FailureKind: "transport", FailureCause: "unavailable", StateVersion: "version", Metrics: &protocol.DecisionMetrics{Version: 1}},
		"request":  protocol.GraphRequest{RequestID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Op: "read_graph", Section: "facts", Offset: 2, ByteOffset: new(int), RecordVersion: "record", Limit: 3, ExpectedVersion: "state", IDs: []string{"fact"}, AssetIDs: []string{"asset"}, Action: board.StateAction{Op: "fact"}, Batch: &board.DecisionBatch{}, Updates: &board.ExecuteUpdateCursor{}},
		"response": protocol.GraphResponse{RequestID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Result: json.RawMessage(`{"committed":true}`), Error: "error"},
	}
}
