package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"pwnmesh/internal/board"
)

func TestPrepareRejectsMalformedProtocolsBeforeFreezingInput(t *testing.T) {
	for _, kind := range []string{"reason", "explore"} {
		t.Run(kind, func(t *testing.T) {
			f, store := newSnapshotHTTPFixture(t)
			template := snapshotTemplate(f, kind)
			counts := func() map[string]int {
				t.Helper()
				counts := map[string]int{}
				if err := fixtureSQL(store).View(context.Background(), func(tx *fixtureSQLTx) error {
					for _, table := range []string{"pwnmesh_executions", "pwnmesh_input_snapshots"} {
						var count int
						if err := tx.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE project_id=?", f.project).Scan(&count); err != nil {
							return err
						}
						counts[table] = count
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				return counts
			}
			before := counts()
			for _, tc := range invalidExecutionProtocols {
				t.Run(tc.name, func(t *testing.T) {
					var fields map[string]any
					_ = json.Unmarshal(template.Job, &fields)
					fields[tc.field] = tc.value
					invalid := template
					invalid.Job, _ = json.Marshal(fields)
					f.request("POST", f.base()+"/executions/prepare", invalid, true, http.StatusUnprocessableEntity, nil)
				})
			}
			for _, field := range []string{"graph_rpc", "result_contract_version"} {
				t.Run("missing_"+field, func(t *testing.T) {
					var fields map[string]any
					_ = json.Unmarshal(template.Job, &fields)
					delete(fields, field)
					invalid := template
					invalid.Job, _ = json.Marshal(fields)
					f.request("POST", f.base()+"/executions/prepare", invalid, true, http.StatusUnprocessableEntity, nil)
				})
			}
			for table, count := range counts() {
				if count != before[table] {
					t.Fatalf("rejected protocol changed %s row count from %d to %d", table, before[table], count)
				}
			}
			_, job := prepareSnapshot(t, f, template)
			if !job.GraphRPC || job.ResultContractVersion != 2 || (kind == "reason" && job.Decision.Version != 2) {
				t.Fatal("valid current protocol was downgraded")
			}
		})
	}
}

func TestPrepareRejectsExplicitCompatibilityProtocol(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	template := snapshotTemplate(f, "reason")
	var fields map[string]any
	_ = json.Unmarshal(template.Job, &fields)
	fields["graph_rpc"], fields["result_contract_version"] = false, 1
	template.Job, _ = json.Marshal(fields)
	f.request("POST", f.base()+"/executions/prepare", template, true, http.StatusUnprocessableEntity, nil)
	if len(f.executionRecords()) != 0 {
		t.Fatal("old protocol persisted an execution")
	}
}

func TestPrepareReasonRejectsClientSuppliedIntent(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	template := snapshotTemplate(f, "reason")
	for _, intent := range []board.Intent{{}, {ID: "client-invented-step", Description: "CLIENT_INVENTED_DESCRIPTION"}} {
		var fields map[string]any
		_ = json.Unmarshal(template.Job, &fields)
		fields["intent"] = intent
		invalid := template
		invalid.Job, _ = json.Marshal(fields)
		f.request("POST", f.base()+"/executions/prepare", invalid, true, http.StatusUnprocessableEntity, nil)
	}
	_, job := prepareSnapshot(t, f, template)
	if job.Intent != nil {
		t.Fatal("Decide retained client Step input")
	}
}
