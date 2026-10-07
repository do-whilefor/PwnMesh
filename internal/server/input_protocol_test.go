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
			if err := store.Do(context.Background(), func(tx *board.Tx) error {
				for _, table := range []string{"xloom_executions", "xloom_input_snapshots"} {
					var count int
					if err := tx.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE project_id=?", f.project).Scan(&count); err != nil {
						return err
					}
					if count != 0 {
						t.Fatalf("rejected protocol persisted %d rows in %s", count, table)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
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
