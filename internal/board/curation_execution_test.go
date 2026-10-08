package board

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCurationCommitAtomicallySettlesExecutionAndLease(t *testing.T) {
	f := newOrchestrationFixture(t)
	e, input := f.curator("atomic")
	receipt := f.curate(e, input)
	f.do(func(tx *Tx) error {
		current, err := tx.Execution("p", e.ID)
		if err != nil {
			return err
		}
		graph, err := tx.Load("p")
		if err != nil {
			return err
		}
		revoked, err := tx.RunRevoked("p", e.Lease)
		if err == nil && (!receipt.Committed || current.Status != "succeeded" || !revoked || graph.Project.Curator != nil || graph.Project.Reason == nil || graph.Project.Reason.Worker != f.planner.Run || !strings.Contains(string(current.Result), "curated")) {
			t.Fatalf("curation did not settle atomically: execution=%+v project=%+v revoked=%v", current, graph.Project, revoked)
		}
		return err
	})
}

func TestCurationSettlementFailureRollsBackBusinessCommitInsideCallerTransaction(t *testing.T) {
	f := newOrchestrationFixture(t)
	e, input := f.curator("rollback")
	before := f.state()
	f.do(func(tx *Tx) error {
		_, err := tx.Exec("CREATE TRIGGER reject_curation_settlement BEFORE UPDATE OF status ON xloom_executions WHEN NEW.kind='curate' AND NEW.status='succeeded' BEGIN SELECT RAISE(FAIL,'synthetic settlement failure'); END")
		return err
	})
	raw, _ := json.Marshal(CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}})
	f.do(func(tx *Tx) error {
		_, err := tx.StateAction("p", e.Fence(), StateAction{Op: "curate", IdempotencyKey: "rollback", Payload: raw, ExpectedVersion: DecisionStateVersion(input)})
		if err == nil || !strings.Contains(err.Error(), "synthetic settlement failure") {
			t.Fatalf("expected settlement failure: %v", err)
		}
		// Swallowing the action error must not retain its business writes.
		return nil
	})
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("failed settlement changed curation state or leases")
	}
	f.do(func(tx *Tx) error {
		current, err := tx.Execution("p", e.ID)
		if err != nil {
			return err
		}
		if current.Status != "prepared" || len(current.Result) != 0 {
			t.Fatalf("partial execution settlement: %+v", current)
		}
		_, err = tx.CurationReceipt("p", e.Lease)
		requireAPIStatus(t, err, 404)
		return nil
	})
}

func legacyPendingCuration(f *orchestrationFixture, status string) (Execution, json.RawMessage) {
	f.t.Helper()
	e, input := f.curator("legacy")
	f.curate(e, input)
	var retained json.RawMessage
	if status == "result_pending" {
		retained = json.RawMessage(`{"status":"success","text":"{\"accepted\":true,\"data\":{\"curated\":true}}","metrics":{"version":1}}`)
	}
	f.do(func(tx *Tx) error {
		if _, err := tx.Exec("UPDATE xloom_executions SET status=?,result=?,resumes=2 WHERE project_id='p' AND id=?", status, []byte(retained), e.ID); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM xloom_revoked_runs WHERE project_id='p' AND worker=?", e.Lease); err != nil {
			return err
		}
		return nil // A lost dispatcher may also have released the old lease.
	})
	return e, retained
}

func TestLegacyCurationReceiptSettlesWithoutModelRecoveryOrReplacingPendingResult(t *testing.T) {
	for _, status := range []string{"running", "result_pending"} {
		t.Run(status, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			e, retained := legacyPendingCuration(f, status)
			before := f.state()
			for range 2 {
				f.do(func(tx *Tx) error { return tx.CompleteCurationExecution("p", e.Lease) })
			}
			if !reflect.DeepEqual(before, f.state()) {
				t.Fatal("legacy receipt recovery changed business state")
			}
			f.do(func(tx *Tx) error {
				current, err := tx.Execution("p", e.ID)
				if err == nil && (current.Status != "succeeded" || current.Resumes != 2 || len(retained) > 0 && string(current.Result) != string(retained)) {
					t.Fatalf("recovery reran or replaced retained input: %+v", current)
				}
				return err
			})
		})
	}
}

func TestLegacyCurationSettlementCannotCrossManagementBoundary(t *testing.T) {
	for _, boundary := range []string{"stopped", "generation", "revoked", "owner", "terminal"} {
		t.Run(boundary, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			e, _ := legacyPendingCuration(f, "running")
			f.do(func(tx *Tx) error {
				switch boundary {
				case "stopped":
					_, err := tx.Exec("UPDATE projects SET status='stopped' WHERE id='p'")
					return err
				case "generation":
					_, err := tx.RestartProject("p", nil)
					return err
				case "revoked":
					_, err := tx.Exec("INSERT INTO xloom_revoked_runs(project_id,worker) VALUES('p',?)", e.Lease)
					return err
				case "owner":
					_, err := tx.ClaimCurator("p", "other@curator", "new input")
					return err
				default:
					return tx.ExecutionStatus(e, "cancelled", json.RawMessage(`{"status":"failed"}`))
				}
			})
			before := f.state()
			err := f.store.Do(context.Background(), func(tx *Tx) error { return tx.CompleteCurationExecution("p", e.Lease) })
			if err == nil {
				t.Fatal("legacy receipt crossed " + boundary)
			}
			if !reflect.DeepEqual(before, f.state()) {
				t.Fatal("rejected recovery changed state")
			}
		})
	}
}
