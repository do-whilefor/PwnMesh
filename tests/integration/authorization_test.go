//go:build linux

package integration

import (
	"context"
	"encoding/json"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/dispatcher"
	"pwnmesh/internal/worker"
)

// Fixture assignments use the same durable authorization as model decisions.
func authorizeIntegrationStep(t *testing.T, client *dispatcher.Client, project board.Graph, description string) board.Intent {
	t.Helper()
	ctx := context.Background()
	base := "/projects/" + project.Project.ID
	const id = "fixture-plan"
	lease := dispatcher.Lease{Run: "fixture@" + id, Kind: "reason"}
	if err := client.Do(ctx, "POST", base+"/reason/claim", map[string]string{"worker": lease.Run, "trigger": "initial"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	job := worker.Job{RunID: id, Kind: "reason", WorkerType: "go", Graph: board.Graph{Project: project.Project}, GraphRPC: true, ResultContractVersion: 2, Workspace: "/workspace", Budget: config.Task{MaxIntents: 1}}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	execution := board.Execution{ProjectID: project.Project.ID, ID: id, Namespace: "fixture", Backend: "fixture", Kind: "reason", Lease: lease.Run, Job: raw}
	if err := client.Do(ctx, "POST", base+"/executions/prepare", execution, &execution, &lease); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(execution.Job, &job); err != nil || job.Decision == nil {
		t.Fatalf("missing decision input: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{"action": "add", "from": []string{"origin"}, "description": description})
	batch := board.DecisionBatch{ExpectedVersion: job.Decision.StateVersion, Actions: []board.DecisionAction{{Op: "step", Ref: "assigned", Payload: payload}}}
	var receipt board.DecisionReceipt
	if err := client.Do(ctx, "POST", base+"/state/decisions/commit", batch, &receipt, &lease); err != nil {
		t.Fatal(err)
	}
	if err := client.Do(ctx, "GET", base, nil, &project, nil); err != nil {
		t.Fatal(err)
	}
	for _, intent := range project.Intents {
		if intent.ID == receipt.IDs["assigned"] {
			return intent
		}
	}
	t.Fatal("committed assignment missing from project")
	return board.Intent{}
}
