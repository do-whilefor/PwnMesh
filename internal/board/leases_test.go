package board

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestLeaseOperationsPreserveBusinessStateAndOtherOwners(t *testing.T) {
	f := newOrchestrationFixture(t)
	step := f.step("Inspect fixture", "")
	before := f.state()
	f.do(func(tx *Tx) error {
		for _, statement := range []string{
			`CREATE TRIGGER guard_lease_project BEFORE UPDATE OF title,status,bootstrap_enabled ON projects BEGIN SELECT RAISE(ABORT,'lease rewrote project'); END`,
			`CREATE TRIGGER guard_lease_fact BEFORE INSERT ON facts BEGIN SELECT RAISE(ABORT,'lease rewrote facts'); END`,
			`CREATE TRIGGER guard_lease_hint BEFORE INSERT ON hints BEGIN SELECT RAISE(ABORT,'lease rewrote hints'); END`,
			`CREATE TRIGGER guard_lease_intent BEFORE INSERT ON intents BEGIN SELECT RAISE(ABORT,'lease rewrote intents'); END`,
			`CREATE TRIGGER guard_lease_sources BEFORE INSERT ON intent_sources BEGIN SELECT RAISE(ABORT,'lease rewrote sources'); END`,
			`CREATE TRIGGER guard_lease_state BEFORE UPDATE ON pwnmesh_state BEGIN SELECT RAISE(ABORT,'lease rewrote state'); END`,
			`CREATE TRIGGER guard_lease_event BEFORE INSERT ON pwnmesh_state_events BEGIN SELECT RAISE(ABORT,'lease published event'); END`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
		tx.Now = "2026-09-28T08:00:01Z"
		version := DecisionStateVersion(before)
		planner, err := tx.ReasonLease("p", LeaseOperation{Op: "claim", Worker: f.planner.Run, Trigger: "replacement trigger", Fence: f.planner})
		if err != nil {
			return err
		}
		if planner.Reason.Trigger != before.Graph.Project.Reason.Trigger || planner.Reason.Heartbeat != before.Graph.Project.Reason.Heartbeat {
			t.Fatal("idempotent planner claim rewrote its original lease")
		}
		planner, err = tx.ReasonLease("p", LeaseOperation{Op: "heartbeat", Worker: f.planner.Run, Fence: f.planner, ExpectedVersion: &version})
		if err != nil {
			return err
		}
		for _, op := range []string{"claim", "heartbeat", "release"} {
			project, err := tx.CuratorLease("p", LeaseOperation{Op: op, Worker: "curator@lease", Trigger: "reconcile"})
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(project.Reason, planner.Reason) {
				t.Fatal("curator changed the planner lease")
			}
		}
		for _, op := range []string{"heartbeat", "heartbeat", "release", "release"} {
			intent, err := tx.IntentLease("p", step, LeaseOperation{Op: op, Worker: "executor@lease"})
			if err != nil {
				return err
			}
			if Value(intent.Heartbeat) != tx.Now || (op == "release") != (intent.Worker == nil) {
				t.Fatalf("%s lost the step's heartbeat or owner: %+v", op, intent)
			}
		}
		after, err := tx.State("p")
		if err != nil {
			return err
		}
		if after.Revision != before.Revision || after.DecisionRevision != before.DecisionRevision || DecisionStateVersion(after) != version || !reflect.DeepEqual(after.Graph.Project.Reason, planner.Reason) {
			t.Fatal("lease updates changed shared input or another owner")
		}
		return nil
	})
}

func TestLeaseOperationsFenceIdentityRevocationAndState(t *testing.T) {
	f := newOrchestrationFixture(t)
	step := f.step("Inspect fixture", "")
	before := f.state()
	f.do(func(tx *Tx) error {
		_, err := tx.ReasonLease("p", LeaseOperation{Op: "heartbeat", Worker: f.planner.Run, Fence: ExecutionFence{Run: "different", Lease: "reason"}})
		requireAPIStatus(t, err, 403)
		wrongVersion := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		_, err = tx.ReasonLease("p", LeaseOperation{Op: "heartbeat", Worker: f.planner.Run, ExpectedVersion: &wrongVersion})
		requireAPIStatus(t, err, 409)
		_, err = tx.IntentLease("p", step, LeaseOperation{Op: "heartbeat", Worker: "executor", Fence: ExecutionFence{Run: "executor", Lease: "explore", Intent: "different-step"}})
		requireAPIStatus(t, err, 403)
		_, err = tx.IntentLease("p", step, LeaseOperation{Op: "conclude", Worker: "executor"})
		requireAPIStatus(t, err, 404)
		_, err = tx.CuratorLease("p", LeaseOperation{Op: "claim", Worker: "curator", Trigger: "test", Fence: ExecutionFence{Run: "curator", Lease: "reason"}})
		requireAPIStatus(t, err, 403)
		return nil
	})
	if after := f.state(); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected authority or stale state changed a lease")
	}
	f.do(func(tx *Tx) error {
		if _, err := tx.IntentLease("p", step, LeaseOperation{Op: "heartbeat", Worker: "executor"}); err != nil {
			return err
		}
		if _, err := tx.CuratorLease("p", LeaseOperation{Op: "claim", Worker: "curator", Trigger: "test"}); err != nil {
			return err
		}
		return tx.RevokeRuns("p")
	})
	for _, renew := range []func(*Tx) error{
		func(tx *Tx) error {
			_, err := tx.ReasonLease("p", LeaseOperation{Op: "heartbeat", Worker: f.planner.Run})
			return err
		},
		func(tx *Tx) error {
			_, err := tx.IntentLease("p", step, LeaseOperation{Op: "heartbeat", Worker: "executor"})
			return err
		},
		func(tx *Tx) error {
			_, err := tx.CuratorLease("p", LeaseOperation{Op: "heartbeat", Worker: "curator"})
			return err
		},
	} {
		err := f.store.Do(context.Background(), renew)
		requireAPIStatus(t, err, 409)
	}
}

func TestLeaseOperationsRejectIncompleteInputWithoutMutation(t *testing.T) {
	f := newOrchestrationFixture(t)
	step := f.step("Inspect fixture", "")
	before := f.state()
	for _, role := range []string{"reason", "curate", "intent"} {
		for _, op := range []string{"claim", "heartbeat", "release"} {
			if role == "intent" && op == "claim" {
				continue
			}
			inputs := []LeaseOperation{{Op: op, Worker: " \t", Trigger: "test"}}
			if op == "claim" {
				inputs = append(inputs, LeaseOperation{Op: op, Worker: f.planner.Run, Trigger: " \t"})
			}
			for _, input := range inputs {
				t.Run(role+"/"+op, func(t *testing.T) {
					f.do(func(tx *Tx) error {
						tx.Now = "2026-09-28T08:00:05Z"
						var err error
						switch role {
						case "reason":
							_, err = tx.ReasonLease("p", input)
						case "curate":
							_, err = tx.CuratorLease("p", input)
						case "intent":
							_, err = tx.IntentLease("p", step, input)
						}
						requireAPIStatus(t, err, 422)
						return nil // Even a caller that handles rejection sees no writes.
					})
					if after := f.state(); !reflect.DeepEqual(before, after) {
						t.Fatal("invalid lease input changed persisted state")
					}
				})
			}
		}
	}
}

func TestCuratorLeaseRollbackHandledVersionFailure(t *testing.T) {
	f := newOrchestrationFixture(t)
	f.do(func(tx *Tx) error {
		_, err := tx.CuratorLease("p", LeaseOperation{Op: "claim", Worker: "curator", Trigger: "test"})
		return err
	})
	before := f.state()
	f.do(func(tx *Tx) error {
		tx.Now = "2026-09-28T08:00:05Z"
		wrongVersion := strings.Repeat("a", 64)
		_, err := tx.CuratorLease("p", LeaseOperation{Op: "heartbeat", Worker: "curator", ExpectedVersion: &wrongVersion})
		if err == nil {
			t.Fatal("unregistered curator version was accepted")
		}
		return nil
	})
	if after := f.state(); !reflect.DeepEqual(before, after) {
		t.Fatal("handled version rejection renewed the curator lease")
	}
}
