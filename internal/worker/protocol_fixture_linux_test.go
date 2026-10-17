//go:build linux

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"pwnmesh/internal/protocol"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

// Session tests use the real file bridge with an unchanged shared graph.
// Tests exercising graph mutations supply their own bridge instead.
func sessionTestBridge(job Job, runDir string) *draftTestBridge {
	return &draftTestBridge{dir: runDir, handle: func(request GraphRequest) (any, error) {
		switch request.Op {
		case "read_graph":
			state := board.State{Graph: job.Graph}
			if job.State != nil {
				state = *job.State
			} else {
				for _, fact := range job.Graph.Facts {
					state.FactRecords = append(state.FactRecords, board.FactRecord{ID: fact.ID, Description: fact.Description, Status: "valid"})
				}
			}
			return protocol.GraphPage(state, request)
		case "read_updates":
			cursor := board.ExecuteUpdateCursor{ProjectID: job.Graph.Project.ID, Generation: job.Graph.Project.Generation, StepID: job.Intent.ID, RunID: job.RunID}
			if request.Updates != nil {
				cursor = *request.Updates
			}
			return board.ExecuteUpdates{Version: 1, ExecuteUpdateCursor: cursor, FromRevision: cursor.Revision, ToRevision: cursor.Revision, StateVersion: strings.Repeat("a", 64), Complete: true}, nil
		case "decision_receipt":
			return board.DecisionReceipt{Committed: false}, nil
		default:
			return nil, errors.New("unexpected fixture graph request: " + request.Op)
		}
	}}
}

func runTestWorker(ctx context.Context, job Job, options Options) (Result, error) {
	bridge := sessionTestBridge(job, options.RunDir)
	switch sink := options.Output.(type) {
	case nil:
		options.Output = bridge
	case *bytes.Buffer:
		options.Output = io.MultiWriter(sink, bridge)
	case *strings.Builder:
		options.Output = io.MultiWriter(sink, bridge)
	}
	return Run(ctx, job, options)
}

func TestWorkerRejectsRetiredProtocolsBeforeCallingProvider(t *testing.T) {
	for name, run := range map[string]func(context.Context, Job, Options) (Result, error){"worker": Run, "session": runSession} {
		for variant, mutate := range map[string]func(*Job){
			"historical project": func(j *Job) { j.Graph.Project.OrchestrationVersion = 0 },
			"unversioned result": func(j *Job) { j.ResultContractVersion = 0 },
			"old result":         func(j *Job) { j.ResultContractVersion = 1 },
			"offline graph":      func(j *Job) { j.GraphRPC = false },
			"mock backend":       func(j *Job) { j.WorkerType = "mock" },
			"bootstrap role":     func(j *Job) { j.Kind = "bootstrap" },
			"missing decision":   func(j *Job) { j.Decision = nil },
			"old decision":       func(j *Job) { j.Decision.Version = 1 },
		} {
			t.Run(name+"/"+variant, func(t *testing.T) {
				job := outcomeJob(t, "reason")
				mutate(&job)
				provider := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
					t.Fatal("retired protocol reached the model")
					return agent.Message{}, nil
				})
				if result, err := run(context.Background(), job, Options{Provider: provider, RunDir: t.TempDir()}); err == nil || result.Status == "success" {
					t.Fatalf("retired protocol accepted: %+v %v", result, err)
				}
			})
		}
	}
}

func TestRuntimeGraphReadRequiresDispatcherBridge(t *testing.T) {
	for _, kind := range []string{"reason", "explore"} {
		t.Run(kind, func(t *testing.T) {
			job := outcomeJob(t, kind)
			job.GraphRPC = false
			var output bytes.Buffer
			options := Options{RunDir: t.TempDir(), Output: &output}
			configureRoleTools(t, job, &options)
			read := snapshotRuntimeTool(t, options, "read_graph")
			raw, err := read.Execute(context.Background(), json.RawMessage(`{"section":"facts","ids":["origin"]}`))
			if err == nil || !strings.Contains(err.Error(), "requires the dispatcher graph bridge") || raw != "" || output.Len() != 0 {
				t.Fatalf("offline graph read bypassed the bridge: raw=%q err=%v output=%q", raw, err, output.String())
			}
		})
	}
}
