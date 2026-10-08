package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

func TestLegacyCurationReceiptReadIsPureAndApplySettlesWithoutRecovery(t *testing.T) {
	for _, status := range []string{"running", "result_pending"} {
		t.Run(status, func(t *testing.T) {
			f := newCurationHTTPFixture(t)
			_, job := prepareCurator(f)
			f.action("curate", "committed", board.CuratePayload{ThroughRevision: job.InputSnapshot.Revision, Groups: []board.CurateGroup{}})
			var retained json.RawMessage
			if status == "result_pending" {
				retained, _ = json.Marshal(worker.Result{Status: "success", Text: `{"accepted":true,"data":{"curated":true}}`, Metrics: &worker.DecisionMetrics{Version: 1}})
			}
			if err := f.store.Do(context.Background(), func(tx *board.Tx) error {
				if _, err := tx.Exec("UPDATE xloom_executions SET status=?,result=?,resumes=2 WHERE project_id=? AND id=?", status, []byte(retained), f.project, f.run); err != nil {
					return err
				}
				if _, err := tx.Exec("DELETE FROM xloom_revoked_runs WHERE project_id=? AND worker=?", f.project, f.lease); err != nil {
					return err
				}
				_, err := tx.ClaimCurator(f.project, f.lease, "legacy delivery")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			beforeState, beforeExecutions := f.state(), f.executionRecords()
			for range 2 {
				var receipt struct {
					board.StateActionResult
					ExecutionStatus string `json:"execution_status"`
				}
				f.request("GET", f.base()+"/state/curation/receipt", nil, true, http.StatusOK, &receipt)
				if !receipt.Committed || receipt.ExecutionStatus != status {
					t.Fatalf("receipt hid legacy settlement state: %+v", receipt)
				}
			}
			if !reflect.DeepEqual(beforeState, f.state()) || !reflect.DeepEqual(beforeExecutions, f.executionRecords()) {
				t.Fatal("GET receipt changed the pending execution, lease or business state")
			}
			f.apply(http.StatusOK)
			f.apply(http.StatusOK)
			after := f.executionRecords()[0]
			if after.Status != "succeeded" || after.Resumes != 2 || len(retained) > 0 && string(after.Result) != string(retained) || f.state().Graph.Project.Curator != nil {
				t.Fatalf("legacy apply reran or replaced the result: %+v", after)
			}
		})
	}
}

func TestCurationMetricsAreRecordedAfterAtomicSuccess(t *testing.T) {
	f := newCurationHTTPFixture(t)
	_, job := prepareCurator(f)
	f.action("curate", "committed", board.CuratePayload{ThroughRevision: job.InputSnapshot.Revision, Groups: []board.CurateGroup{}})
	before := f.state()
	metrics := &worker.DecisionMetrics{Version: 1}
	for range 2 {
		f.request("POST", f.base()+"/executions/"+f.run+"/observation", map[string]any{"metrics": metrics}, true, http.StatusOK, nil)
	}
	var result worker.Result
	if json.Unmarshal(f.executionRecords()[0].Result, &result) != nil || !reflect.DeepEqual(result.Metrics, metrics) || !reflect.DeepEqual(before, f.state()) {
		t.Fatal("post-commit metrics changed curation or were lost")
	}
}
