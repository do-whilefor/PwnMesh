package sqlstore

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestLifecycleExecutionRowsKeepScopeAndSavedResults(t *testing.T) {
	store := stateEventStore(t, ":memory:")
	ctx := context.Background()
	statuses := []string{"prepared", "running", "retryable", "result_pending", "retry_requested", "failed", "cancelled", "rejected", "succeeded", "retried"}
	if err := store.Do(ctx, func(q Queries) error {
		for _, project := range []string{"p", "other"} {
			for _, status := range statuses {
				e := model.Execution{ProjectID: project, ID: status, Namespace: "ns", Backend: "worker", Kind: "explore", Intent: "step", Lease: "worker@" + status, RetryKey: "key", Status: status, Job: []byte("{ \"retained\": true }\n"), Result: []byte("{ \"status\": \"failed\" }\n"), Resumes: 2, CreatedAt: "created", UpdatedAt: "updated"}
				if err := InsertExecution(q, e, model.ExecutionSummary{}, ""); err != nil {
					return err
				}
			}
		}
		if _, err := q.Exec("INSERT INTO pwnmesh_revoked_runs VALUES('p','worker@running')"); err != nil {
			return err
		}
		for _, project := range []string{"p", "other", "p"} {
			if err := MarkPausedExecutions(q, project); err != nil {
				return err
			}
		}
		for _, project := range []string{"p", "other", "missing"} {
			paused, err := PausedExecutionIDs(q, project)
			if err != nil {
				return err
			}
			pending, err := PendingProjectExecutionIDs(q, project)
			if err != nil {
				return err
			}
			wantPaused := []string{"prepared", "result_pending", "retryable", "running"}
			wantPending := slices.Clone(wantPaused)
			if project == "p" {
				wantPaused = wantPaused[:3]
			} else if project == "missing" {
				wantPaused, wantPending = []string{}, []string{}
			}
			slices.Sort(paused)
			slices.Sort(pending)
			if !reflect.DeepEqual(paused, wantPaused) || !reflect.DeepEqual(pending, wantPending) {
				t.Fatalf("wrong pause or pending scope for %s: paused=%v pending=%v", project, paused, pending)
			}
		}
		var before []model.Execution
		for _, project := range []string{"p", "other"} {
			if err := VisitExecutions(q, project, func(e model.Execution) error { before = append(before, e); return nil }); err != nil {
				return err
			}
		}
		if err := CancelProjectRetries(q, "p", "terminated"); err != nil {
			return err
		}
		if err := DeletePausedExecutions(q, "p"); err != nil {
			return err
		}
		if err := RecordProjectTermination(q, "p", "terminated"); err != nil {
			return err
		}
		for _, want := range before {
			if want.ProjectID == "p" && want.Status == "retry_requested" {
				want.Status, want.UpdatedAt = "cancelled", "terminated"
			}
			got, err := Execution(q, want.ProjectID, want.ID)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("lifecycle changed another execution field: got %+v, want %+v, error %v", got, want, err)
			}
		}
		for _, project := range []string{"p", "other"} {
			ids, err := PausedExecutionIDs(q, project)
			if err != nil {
				return err
			}
			want := 4
			if project == "p" {
				want = 0
			}
			if len(ids) != want {
				t.Fatalf("pause deletion crossed project %s: %v", project, ids)
			}
		}
		var termination string
		var markers, events int
		if err := q.QueryRow("SELECT terminated_at,(SELECT COUNT(*) FROM pwnmesh_project_termination),(SELECT COUNT(*) FROM pwnmesh_state_events) FROM pwnmesh_project_termination WHERE project_id='p'").Scan(&termination, &markers, &events); err != nil {
			return err
		}
		if termination != "terminated" || markers != 1 || events != 0 {
			t.Fatalf("termination changed scope or invented failure events: %q, %d, %d", termination, markers, events)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResetProjectRoundRetainsHistoryAndHumanInputs(t *testing.T) {
	store := stateEventStore(t, ":memory:")
	ctx := context.Background()
	retained := []string{"projects", "hints", "scoped_counters", "pwnmesh_revoked_runs", "pwnmesh_round_history", "pwnmesh_round_entries", "pwnmesh_input_files", "pwnmesh_input_batches", "pwnmesh_input_snapshots", "pwnmesh_snapshot_chunks", "pwnmesh_assets", "pwnmesh_asset_anchors"}
	cleared := []string{"intents", "intent_sources", "pwnmesh_state", "pwnmesh_state_actions", "pwnmesh_state_events", "pwnmesh_executions", "pwnmesh_paused_executions", "pwnmesh_decision_read_views", "pwnmesh_project_termination", "pwnmesh_ui_execution_changes"}
	tables := append(append(slices.Clone(retained), cleared...), "facts", "pwnmesh_project_rounds", "pwnmesh_ui_execution_revisions")
	if err := store.Do(ctx, func(q Queries) error {
		for _, project := range []string{"p", "other"} {
			if err := InsertExecution(q, model.Execution{ProjectID: project, ID: "run", Lease: "worker@run", Status: "running", Job: []byte("retained job")}, model.ExecutionSummary{}, ""); err != nil {
				return err
			}
			for _, statement := range []string{
				"INSERT INTO facts VALUES('origin',?,'Human input')",
				"INSERT INTO facts VALUES('goal',?,'Human goal')",
				"INSERT INTO facts VALUES('proof',?,'Observed proof')",
				"INSERT INTO intents(id,project_id,description,creator,created_at) VALUES('step',?,'Observe','planner','created')",
				"INSERT INTO intent_sources VALUES('step',?,'origin')",
				"INSERT INTO hints VALUES('hint',?,'Human hint','user','created')",
				"INSERT INTO scoped_counters VALUES(?,'fact',17)",
				"INSERT INTO pwnmesh_revoked_runs VALUES(?,'old-worker')",
				"INSERT INTO pwnmesh_state VALUES(?,'{ \"retained\": 1 }',4,3)",
				"INSERT INTO pwnmesh_state_actions VALUES(?,'action','request bytes','response bytes')",
				"INSERT INTO pwnmesh_state_events VALUES(?,4,'event bytes')",
				"INSERT INTO pwnmesh_paused_executions VALUES(?,'run')",
				"INSERT INTO pwnmesh_decision_read_views VALUES(?,'run','version',X'0001')",
				"INSERT INTO pwnmesh_project_termination VALUES(?,'terminated')",
				"INSERT INTO pwnmesh_project_rounds VALUES(?,2,'previous')",
				"INSERT INTO pwnmesh_round_history VALUES(?,2,'archived')",
				"INSERT INTO pwnmesh_round_entries VALUES(?,2,'state','state',X'00FF','digest')",
				"INSERT INTO pwnmesh_input_files VALUES(?,'input','input.txt',2,'digest','created',X'00FE')",
				"INSERT INTO pwnmesh_input_batches VALUES(?,'batch','digest','receipt')",
				"INSERT INTO pwnmesh_input_snapshots VALUES(?,'snapshot',X'00FD',X'00FC')",
				"INSERT INTO pwnmesh_snapshot_chunks VALUES(?,'digest',X'00FB')",
				"INSERT INTO pwnmesh_assets VALUES(?,'asset','url','https://fixture.invalid','')",
				"INSERT INTO pwnmesh_asset_anchors VALUES(?,2,'fact','proof','asset')",
			} {
				if _, err := q.Exec(statement, project); err != nil {
					return err
				}
			}
		}
		if _, err := q.Exec("UPDATE counters SET value=19 WHERE name='project'"); err != nil {
			return err
		}
		before := lifecycleRows(t, q, "p", tables)
		other := lifecycleRows(t, q, "other", tables)
		if err := ResetProjectRound(q, "p", 3, "restarted"); err != nil {
			return err
		}
		after := lifecycleRows(t, q, "p", tables)
		if got := lifecycleRows(t, q, "other", tables); !reflect.DeepEqual(got, other) {
			t.Fatal("round reset changed another project's rows")
		}
		for _, table := range retained {
			if !reflect.DeepEqual(after[table], before[table]) {
				t.Errorf("round reset changed retained %s bytes", table)
			}
		}
		for _, table := range cleared {
			if len(after[table]) != 0 {
				t.Errorf("round reset retained current %s rows: %v", table, after[table])
			}
		}
		if !reflect.DeepEqual(after["facts"], before["facts"][:2]) {
			t.Errorf("round reset changed origin/goal or retained observations: %v", after["facts"])
		}
		if !reflect.DeepEqual(after["pwnmesh_project_rounds"], [][]any{{"p", int64(3), "restarted"}}) {
			t.Errorf("wrong new round: %v", after["pwnmesh_project_rounds"])
		}
		if !reflect.DeepEqual(after["pwnmesh_ui_execution_revisions"], [][]any{{"p", int64(2)}}) {
			t.Errorf("execution deletion reset the UI sequence: %v", after["pwnmesh_ui_execution_revisions"])
		}
		var counter int
		if err := q.QueryRow("SELECT value FROM counters WHERE name='project'").Scan(&counter); err != nil {
			return err
		}
		if counter != 19 {
			t.Errorf("round reset changed project allocation: %d", counter)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func lifecycleRows(t *testing.T, q Queries, project string, tables []string) map[string][][]any {
	t.Helper()
	out := make(map[string][][]any, len(tables))
	for _, table := range tables {
		column := "project_id"
		if table == "projects" {
			column = "id"
		}
		rows, err := q.Query("SELECT * FROM "+table+" WHERE "+column+"=? ORDER BY rowid", project)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values, pointers := make([]any, len(columns)), make([]any, len(columns))
			for n := range values {
				pointers[n] = &values[n]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			out[table] = append(out[table], values)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}
