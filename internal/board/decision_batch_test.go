package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDecisionBatchReusesPersistedTransitionsThroughCompletion(t *testing.T) {
	f := newStateBenchmarkFixture(t, stateBenchmarkSize{16, 128, 2}, true)
	batch := DecisionBatch{ExpectedVersion: f.version, Actions: []DecisionAction{
		{Op: "step", Payload: json.RawMessage(`{"action":"abandon","id":"active","reason":"finish this round"}`)},
		{Op: "goal", Ref: "child", Payload: json.RawMessage(`{"action":"add","condition":"Review alternate path"}`)},
		{Op: "step", Ref: "work", Payload: json.RawMessage(`{"action":"add","goal_id":"$child","from":["source000"],"description":"Check alternate path"}`)},
		{Op: "step", Payload: json.RawMessage(`{"action":"abandon","id":"$work","reason":"alternative ruled out"}`)},
		{Op: "goal", Payload: json.RawMessage(`{"action":"withdraw","id":"$child","reason":"alternative ruled out"}`)},
		{Op: "complete", Payload: json.RawMessage(`{"from":["source001"],"description":"Fixture reviewed"}`)},
	}}
	if err := f.store.Do(context.Background(), func(tx *Tx) error {
		preview, err := tx.PreviewDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch)
		if err != nil {
			return err
		}
		if preview.Committed || preview.ChangedActions != 6 || preview.CompletionReview == nil {
			t.Fatalf("preview did not see preceding actions: %+v", preview)
		}
		state, err := tx.State(stateBenchmarkProject)
		if err != nil {
			return err
		}
		if DecisionStateVersion(state) != f.version || state.Revision != 0 {
			t.Fatal("preview escaped its savepoint")
		}
		result, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch)
		if err != nil {
			return err
		}
		if !result.Completed || !result.Committed || result.ChangedActions != 6 {
			t.Fatalf("completion did not observe the resolved Step and Goal: %+v", result)
		}
		state, err = tx.State(stateBenchmarkProject)
		if err != nil {
			return err
		}
		if state.Graph.Project.Reason != nil || state.Graph.Project.Status != "completed" || state.Revision != 6 || result.StateVersion != DecisionStateVersion(state) {
			t.Fatalf("batch result differs from persisted final state: %+v", result)
		}
		replay, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch)
		if err == nil && (replay.StateVersion != result.StateVersion || replay.IDs["work"] != result.IDs["work"]) {
			t.Fatal("receipt replay changed batch")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

type decisionReleaseFailureQueries struct {
	transactionQueries
	failure error
}

func (q *decisionReleaseFailureQueries) Exec(query string, args ...any) (sql.Result, error) {
	if query == "RELEASE pwnmesh_decision_batch" && q.failure != nil {
		err := q.failure
		q.failure = nil
		return nil, err
	}
	return q.transactionQueries.Exec(query, args...)
}

func TestDecisionBatchReleaseFailurePreservesCallerAndRetry(t *testing.T) {
	for _, commit := range []bool{false, true} {
		name := "preview"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			f := newStateBenchmarkFixture(t, stateBenchmarkSize{16, 128, 2}, true)
			failure := errors.New("decision savepoint release failed")
			var rejected DecisionReceipt
			if err := f.store.Do(context.Background(), func(tx *Tx) error {
				original := tx.transactionQueries
				tx.transactionQueries = &decisionReleaseFailureQueries{original, failure}
				var err error
				if commit {
					rejected, err = tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, f.batch(2))
				} else {
					rejected, err = tx.PreviewDecision(stateBenchmarkProject, stateBenchmarkPlanner, f.batch(2))
				}
				tx.transactionQueries = original
				if !errors.Is(err, failure) {
					t.Fatalf("release failure was lost: %v", err)
				}
				return tx.UpdateSettings(Settings{IntentTimeout: 25, ReasonTimeout: 35})
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.store.Do(context.Background(), func(tx *Tx) error {
				state, err := tx.State(stateBenchmarkProject)
				if err != nil {
					return err
				}
				var count int
				if err := tx.transactionQueries.QueryRow("SELECT (SELECT COUNT(*) FROM pwnmesh_state_events)+(SELECT COUNT(*) FROM pwnmesh_state_actions)").Scan(&count); err != nil {
					return err
				}
				if count != 0 || state.Revision != 0 || DecisionStateVersion(state) != f.version || state.Graph.Project.Reason == nil {
					t.Fatal("handled release failure committed a decision or released its lease")
				}
				if !reflect.DeepEqual(rejected, emptyDecisionReceipt()) {
					t.Fatal("failed decision returned uncommitted results")
				}
				settings, err := tx.Settings()
				if err != nil || settings.IntentTimeout != 25 || settings.ReasonTimeout != 35 {
					t.Fatalf("decision rollback discarded caller writes: %+v %v", settings, err)
				}
				result, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, f.batch(2))
				if err == nil && (!result.Committed || result.IDs["step0"] != "i017" || result.IDs["step1"] != "i018") {
					t.Fatal("failed decision consumed counters or prevented retry")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDecisionBatchSQLFailureDiscardsReusableStateAndCounters(t *testing.T) {
	f := newStateBenchmarkFixture(t, stateBenchmarkSize{16, 128, 2}, true)
	batch := f.batch(2)
	if err := f.store.Do(context.Background(), func(tx *Tx) error {
		if _, err := tx.transactionQueries.Exec(`CREATE TRIGGER reject_second_batch_event BEFORE INSERT ON pwnmesh_state_events WHEN NEW.revision=2 BEGIN SELECT RAISE(ABORT,'event unavailable'); END`); err != nil {
			return err
		}
		if _, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch); err == nil {
			t.Fatal("injected SQL failure accepted")
		}
		assertDirectDecisionWritesRejected(t, tx, stateBenchmarkProject, stateBenchmarkPlanner)
		state, err := tx.State(stateBenchmarkProject)
		if err != nil {
			return err
		}
		var count int
		if err := tx.transactionQueries.QueryRow("SELECT (SELECT COUNT(*) FROM pwnmesh_state_events)+(SELECT COUNT(*) FROM pwnmesh_state_actions)").Scan(&count); err != nil {
			return err
		}
		if count != 0 || state.Revision != 0 || DecisionStateVersion(state) != f.version || state.Graph.Project.Reason == nil {
			t.Fatal("failure retained graph, event, receipt or released lease")
		}
		if _, err := tx.transactionQueries.Exec("DROP TRIGGER reject_second_batch_event"); err != nil {
			return err
		}
		result, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch)
		if err == nil && (result.IDs["step0"] != "i017" || result.IDs["step1"] != "i018") {
			t.Fatalf("failed batch consumed counters: %+v", result)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDecisionBatchSavepointProtectsCallerThatHandlesFailure(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "batch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = store.Do(context.Background(), func(tx *Tx) error {
		fence := ExecutionFence{Run: "planner@batch-run", Lease: "reason"}
		g := Graph{Project: Project{ID: "project", Title: "Fixture", Status: "active", CreatedAt: tx.Now, OrchestrationVersion: 1, Reason: &Reason{Worker: fence.Run, Trigger: "initial", StartedAt: tx.Now, Heartbeat: tx.Now}}, Facts: []Fact{{ID: "origin", Description: "Fixture input"}, {ID: "goal", Description: "Inspect fixture"}}, Intents: []Intent{}, Hints: []Hint{}}
		if err := tx.Save(g); err != nil {
			return err
		}
		state, err := tx.State(g.Project.ID)
		if err != nil {
			return err
		}
		job, _ := json.Marshal(map[string]any{"kind": "reason", "run_id": "batch-run", "graph": g, "state": state, "graph_rpc": true, "result_contract_version": 2, "decision": map[string]any{"version": 2, "state_version": DecisionStateVersion(state)}, "budget": map[string]int{"max_intents": 3}})
		if err = tx.RegisterExecution(Execution{ProjectID: g.Project.ID, ID: "batch-run", Namespace: "fixture", Backend: "planner", Kind: "reason", Lease: fence.Run, Job: job, RetryKey: "reason:fixture"}); err != nil {
			return err
		}
		batch := DecisionBatch{ExpectedVersion: DecisionStateVersion(state), Actions: []DecisionAction{
			{Op: "goal", Ref: "temporary", Payload: json.RawMessage(`{"action":"add","condition":"Temporary condition"}`)},
			{Op: "step", Payload: json.RawMessage(`{"action":"add","from":["missing"],"description":"Invalid source"}`)},
		}}
		if _, err = tx.CommitDecision(g.Project.ID, fence, batch); err == nil {
			t.Fatal("invalid batch succeeded")
		}
		assertDirectDecisionWritesRejected(t, tx, g.Project.ID, fence)
		current, err := tx.State(g.Project.ID)
		if err != nil {
			return err
		}
		if DecisionStateVersion(current) != batch.ExpectedVersion || current.Revision != state.Revision {
			t.Fatal("handled failure left partial changes inside caller transaction")
		}
		batch.Actions = batch.Actions[:1]
		batch.Actions = append(batch.Actions, DecisionAction{Op: "goal", Ref: "same", Payload: batch.Actions[0].Payload})
		batch.Actions = append(batch.Actions, DecisionAction{Op: "step", Payload: json.RawMessage(`{"action":"add","goal_id":"$temporary","from":["origin"],"description":"Investigate the temporary condition"}`)})
		preview, err := tx.PreviewDecision(g.Project.ID, fence, batch)
		if err != nil {
			return err
		}
		if preview.Committed || preview.ChangedActions != 2 || len(preview.Results) != 3 || !preview.Results[1].Unchanged {
			t.Fatalf("preview changed-action count includes a duplicate: %+v", preview)
		}
		assertDirectDecisionWritesRejected(t, tx, g.Project.ID, fence)
		current, err = tx.State(g.Project.ID)
		if err != nil {
			return err
		}
		if current.Revision != state.Revision || len(current.Goals) != 1 || len(current.Steps) != 0 {
			t.Fatal("preview left staged goals, steps or events")
		}
		result, err := tx.CommitDecision(g.Project.ID, fence, batch)
		if err != nil {
			return err
		}
		if !result.Committed || result.IDs["temporary"] != "g001" || result.ChangedActions != 2 {
			t.Fatalf("rollback consumed alias ID allocation: %+v", result)
		}
		assertDirectDecisionWritesRejected(t, tx, g.Project.ID, fence)
		saved, err := tx.DecisionReceipt(g.Project.ID, fence)
		if err != nil {
			return err
		}
		if !saved.Committed || saved.ChangedActions != result.ChangedActions || len(saved.Results) != 3 {
			t.Fatalf("durable receipt lost changed-action count: %+v", saved)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertDirectDecisionWritesRejected(t *testing.T, tx *Tx, project string, fence ExecutionFence) {
	t.Helper()
	_, actionErr := tx.StateAction(project, fence, StateAction{Op: "goal", IdempotencyKey: "outside-batch", Payload: json.RawMessage(`{"action":"add","condition":"Must remain inside a batch"}`)})
	_, completeErr := tx.StateAction(project, fence, StateAction{Op: "complete", IdempotencyKey: "outside-completion", Payload: json.RawMessage(`{"from":["origin"],"description":"Must remain inside a batch"}`)})
	for _, err := range []error{actionErr, completeErr} {
		var api *APIError
		if !errors.As(err, &api) || api.Status != 403 || api.Detail != "Registered Decide must submit its plan through a decision batch" {
			t.Fatalf("direct write escaped the batch boundary: %v", err)
		}
	}
}

func TestStandaloneStateActionRetainsVersionAndCompletionChecks(t *testing.T) {
	f := newStateBenchmarkFixture(t, stateBenchmarkSize{1, 128, 1}, false)
	if err := f.store.Do(context.Background(), func(tx *Tx) error {
		action := f.action("step", 0)
		for _, tc := range []struct {
			op, version string
			status      int
		}{
			{"complete", f.version, 422},
			{"step", "stale", 409},
		} {
			action.Op, action.ExpectedVersion = tc.op, tc.version
			_, err := tx.StateAction(stateBenchmarkProject, stateBenchmarkPlanner, action)
			var api *APIError
			if !errors.As(err, &api) || api.Status != tc.status {
				t.Fatalf("standalone %s with version %q: %v", tc.op, tc.version, err)
			}
		}
		action.Op, action.ExpectedVersion = "step", f.version
		if _, err := tx.StateAction(stateBenchmarkProject, stateBenchmarkPlanner, action); err != nil {
			return err
		}
		// A successful direct action must not authorize a later stale action.
		_, err := tx.StateAction(stateBenchmarkProject, stateBenchmarkPlanner, f.action("step", 1))
		var api *APIError
		if !errors.As(err, &api) || api.Status != 409 {
			t.Fatalf("standalone action skipped the current version check: %v", err)
		}
		state, err := tx.State(stateBenchmarkProject)
		if err == nil && state.Revision != 1 {
			t.Fatalf("rejected direct actions retained changes: revision=%d", state.Revision)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRetiredDecisionCannotRegisterReadOrWrite(t *testing.T) {
	for _, input := range []string{"inline", "snapshot"} {
		t.Run(input, func(t *testing.T) {
			f := newStateBenchmarkFixture(t, stateBenchmarkSize{1, 128, 1}, true)
			if err := f.store.Do(context.Background(), func(tx *Tx) error {
				execution, err := tx.Execution(stateBenchmarkProject, "benchmark")
				if err != nil {
					return err
				}
				state, err := tx.State(stateBenchmarkProject)
				if err != nil {
					return err
				}
				var job map[string]any
				if err := json.Unmarshal(execution.Job, &job); err != nil {
					return err
				}
				if input == "snapshot" {
					ref, err := tx.FreezeInput(state)
					if err != nil {
						return err
					}
					delete(job, "state")
					job["input_snapshot"] = ref
				}
				// Both current input forms retain their immutable version binding.
				current, _ := json.Marshal(job)
				if version, err := DecisionJobVersion(current); err != nil || version != f.version {
					t.Fatalf("current %s decision rejected: version=%s err=%v", input, version, err)
				}
				for version, status := range map[string]int{"": 422, "stale": 409} {
					batch := f.batch(1)
					batch.ExpectedVersion = version
					_, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch)
					requireAPIStatus(t, err, status)
				}
				job["decision"].(map[string]any)["version"] = 1
				execution.Job, _ = json.Marshal(job)
				requireAPIStatus(t, tx.RegisterExecution(execution), 422)
				// Seed a retained record without passing through live registration.
				if _, err := tx.transactionQueries.Exec("UPDATE pwnmesh_executions SET job=? WHERE project_id=? AND id=?", string(execution.Job), execution.ProjectID, execution.ID); err != nil {
					return err
				}
				assertDirectDecisionWritesRejected(t, tx, stateBenchmarkProject, stateBenchmarkPlanner)
				for _, refresh := range []bool{false, true} {
					_, err := tx.DecisionReadState(state, stateBenchmarkPlanner, refresh)
					requireAPIStatus(t, err, 422)
				}
				_, err = tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, f.batch(1))
				requireAPIStatus(t, err, 403)
				retained, err := tx.Execution(stateBenchmarkProject, execution.ID)
				if err == nil && string(retained.Job) != string(execution.Job) {
					t.Fatal("rejecting the retired protocol changed its retained job")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
