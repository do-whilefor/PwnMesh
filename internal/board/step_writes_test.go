package board

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/artifactcheck"
)

func writingStep(f *orchestrationFixture, label string, paths ...string) string {
	f.t.Helper()
	return f.action(f.planner, "step", "write:"+label, map[string]any{"action": "add", "description": label, "from": []string{"origin"}, "write_paths": paths}, "").ID
}

func TestStepWritePathsNormalizePersistAndRemainImmutable(t *testing.T) {
	f := newOrchestrationFixture(t)
	id := writingStep(f, "report", "/workspace/results/../report.json", "/workspace//report.json", "/workspace/cache/")
	step := dependencyStepState(t, f.state(), id)
	want := []string{"/workspace/cache", "/workspace/report.json"}
	if !slices.Equal(step.WritePaths, want) {
		t.Fatalf("write declaration was not canonicalized and persisted: %+v", step)
	}
	again := f.action(f.planner, "step", "same-writes", map[string]any{"action": "add", "description": "report", "from": []string{"origin"}, "write_paths": []string{"/workspace/report.json", "/workspace/cache"}}, "")
	if again.ID != id || !again.Unchanged {
		t.Fatal("equivalent declarations created a second task")
	}
	changed := f.action(f.planner, "step", "different-writes", map[string]any{"action": "add", "description": "report", "from": []string{"origin"}, "write_paths": []string{"/workspace/different.json"}}, "")
	if changed.ID == id {
		t.Fatal("different output ownership was silently deduplicated")
	}
	for _, action := range []string{"priority", "abandon", "retry"} {
		raw, _ := json.Marshal(map[string]any{"action": action, "id": id, "reason": "change outputs", "write_paths": []string{}})
		err := f.store.Do(context.Background(), func(tx *Tx) error {
			_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: action, Payload: raw})
			return err
		})
		requireAPIStatus(t, err, 422)
	}
	legacy := f.step("old task without declarations", "")
	if dependencyStepState(t, f.state(), legacy).WritePaths != nil {
		t.Fatal("legacy task acquired an invented output contract")
	}
	spec := &artifactcheck.Spec{Path: "/workspace/report.json", SHA256: strings.Repeat("a", 64), Rules: []artifactcheck.Rule{{Kind: "json_valid"}}}
	repair := f.action(f.planner, "step", "repair-writes", map[string]any{"action": "add", "description": "Repair output", "from": []string{"origin"}, "repair": spec}, "")
	if !slices.Equal(dependencyStepState(t, f.state(), repair.ID).WritePaths, []string{spec.Path}) {
		t.Fatal("repair target did not reserve its output")
	}
}

func TestStepWritePathsRejectEscapesAndUnboundedDeclarations(t *testing.T) {
	for _, paths := range [][]string{{"relative/file"}, {"/workspace/../../tmp/file"}, {"/workspace"}, {"/workspace/.pwnmesh/../.pwnmesh/runs/x"}, {"/workspace/a\\b"}, {"/workspace/a\n"}, {"/workspace/" + strings.Repeat("a", 4097)}, make([]string, 17)} {
		if _, err := normalizeStepWritePaths(paths, nil); err == nil {
			t.Fatalf("accepted invalid writes: %q", paths)
		}
	}
}

func TestStepWriteClaimsSerializeAcrossStoresWithoutBlockingIndependentWork(t *testing.T) {
	f := newOrchestrationFixture(t)
	ids := []string{writingStep(f, "directory", "/workspace/output"), writingStep(f, "file", "/workspace/output/./report.json")}
	independent := writingStep(f, "independent", "/workspace/output-other/report.json")
	other, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.Now = f.store.Now
	stores := []*Store{f.store, other}
	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for n := range ids {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			errs[n] = stores[n].Do(context.Background(), func(tx *Tx) error {
				if err := tx.StepHeartbeatReady("p", ids[n], ""); err != nil {
					return err
				}
				_, err := tx.Exec("UPDATE intents SET worker=?,last_heartbeat_at=? WHERE project_id='p' AND id=?", "writer@"+ids[n], tx.Now, ids[n])
				return err
			})
		}(n)
	}
	close(start)
	wg.Wait()
	winner, loser := 0, 1
	if errs[0] != nil {
		winner, loser = 1, 0
	}
	if errs[winner] != nil {
		t.Fatalf("no writer acquired its scope: %v", errs)
	}
	requireAPIStatus(t, errs[loser], 409)
	f.do(func(tx *Tx) error {
		if err := tx.StepHeartbeatReady("p", ids[winner], "writer@"+ids[winner]); err != nil {
			return fmt.Errorf("waiting peer invalidated active writer heartbeat: %w", err)
		}
		if err := tx.StepReady("p", independent); err != nil {
			return fmt.Errorf("independent output was blocked: %w", err)
		}
		state, err := tx.State("p")
		if err != nil {
			return err
		}
		// Simulate a scheduling page containing only the waiting and independent
		// tasks. Its owner is on an earlier page and must still reserve the path.
		var intents []Intent
		var steps []Step
		for _, intent := range state.Graph.Intents {
			if intent.ID != ids[winner] {
				intents = append(intents, intent)
				steps = append(steps, dependencyStepState(t, state, intent.ID))
			}
		}
		checks, err := tx.ScheduleExecutionChecks("p", "test", intents, steps)
		if err != nil {
			return err
		}
		if !checks["explore:"+ids[loser]].Blocked || checks["explore:"+independent].Blocked {
			t.Fatalf("write wait hid independent work: %+v", checks)
		}
		check, err := tx.CheckExecutions(ExecutionCheckQuery{ProjectID: "p", Namespace: "test", Kind: "explore", Intent: ids[loser], RetryKey: "explore:" + ids[loser]})
		if err == nil && !check.Blocked {
			t.Fatal("targeted scheduling bypassed output ownership")
		}
		return err
	})
	f.do(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE intents SET worker=NULL WHERE project_id='p' AND id=?", ids[winner])
		if err != nil {
			return err
		}
		return tx.StepReady("p", ids[loser])
	})
}

func TestStepWritesPreserveRetryAndAcceptedDependencyHandoff(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := writingStep(f, "produce shared output", "/workspace/report.json")
	consumer := f.action(f.planner, "step", "merge", map[string]any{"action": "add", "description": "Merge checked output", "from": []string{"origin"}, "depends_on": []string{producer}, "write_paths": []string{"/workspace/report.json"}}, "").ID
	first := f.worker("first-writer", producer)
	f.do(func(tx *Tx) error {
		requireAPIStatus(t, tx.StepReady("p", consumer), 409)
		return tx.ExecutionStatus(first, "failed", json.RawMessage(`{"status":"failed","error":"fixture failure"}`))
	})
	grantMainRetry(f, "retry-writing", first)
	next := retrySuccessor(f, first, "next-writer")
	if !slices.Equal(dependencyStepState(t, f.state(), producer).WritePaths, []string{"/workspace/report.json"}) {
		t.Fatal("retry discarded immutable output ownership")
	}
	fact := f.fact(next, "checked-result")
	f.finish(next, fact)
	f.do(func(tx *Tx) error { return tx.StepReady("p", consumer) })
	consumerRun := dependencyRun(f, "merge-writer", consumer)
	f.do(func(tx *Tx) error { return tx.RegisterExecution(consumerRun) })
	f.do(func(tx *Tx) error { return tx.ExecutionStatus(consumerRun, "running", nil) })
}

func TestStepWritesRetainPendingReservationAfterLeaseEnds(t *testing.T) {
	for _, transition := range []string{"abandon", "revoke_and_release", "expire", "result_pending"} {
		t.Run(transition, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			writer := writingStep(f, "original writer", "/workspace/shared/report.json")
			waiting := writingStep(f, "next writer", "/workspace/shared")
			independent := writingStep(f, "other output", "/workspace/other/report.json")
			run := f.worker("original", writer)
			if transition != "revoke_and_release" {
				f.do(func(tx *Tx) error { return tx.ExecutionStatus(run, "running", nil) })
			}
			if transition == "expire" {
				f.do(func(tx *Tx) error {
					return tx.ExecutionStatus(run, "retryable", json.RawMessage(`{"status":"failed","retryable":true}`))
				})
			}
			switch transition {
			case "abandon":
				f.action(f.planner, "step", "abandon-writer", map[string]any{"action": "abandon", "id": writer, "reason": "replace the direction"}, "")
			case "revoke_and_release":
				f.do(func(tx *Tx) error {
					if err := tx.RevokeRuns("p"); err != nil {
						return err
					}
					_, err := tx.Exec("UPDATE intents SET worker=NULL WHERE project_id='p' AND id=?", writer)
					return err
				})
			case "expire":
				now := f.store.Now().Add(time.Minute)
				f.store.Now = func() time.Time { return now }
				f.do(func(tx *Tx) error { return tx.ExpireProject("p") })
			case "result_pending":
				fact := f.fact(run, "accepted-output")
				f.do(func(tx *Tx) error {
					if _, err := tx.ConcludeEvidenceStep("p", run.Fence(), fact, nil); err != nil {
						return err
					}
					return tx.ExecutionStatus(run, "result_pending", json.RawMessage(`{"status":"success","text":"completed"}`))
				})
				if dependencyStepState(t, f.state(), writer).Result == nil {
					t.Fatal("fixture did not conclude the Step before result delivery settled")
				}
			}
			checkBlocked := func(want bool) {
				t.Helper()
				f.do(func(tx *Tx) error {
					err := tx.StepReady("p", waiting)
					if want {
						requireAPIStatus(t, err, 409)
					} else if err != nil {
						return err
					}
					if err := tx.StepReady("p", independent); err != nil {
						return fmt.Errorf("pending reservation blocked independent output: %w", err)
					}
					check, err := tx.CheckExecutions(ExecutionCheckQuery{ProjectID: "p", Namespace: "test", Kind: "explore", Intent: waiting, RetryKey: "explore:" + waiting})
					if err != nil {
						return err
					}
					state, err := tx.State("p")
					if err != nil {
						return err
					}
					checks, err := tx.ScheduleExecutionChecks("p", "test", state.Graph.Intents, state.Steps)
					if err == nil && (check.Blocked != want || checks["explore:"+waiting].Blocked != want || checks["explore:"+independent].Blocked) {
						t.Fatalf("admission paths disagree about pending ownership: single=%+v batch=%+v want=%v", check, checks, want)
					}
					return err
				})
			}
			checkBlocked(true)
			f.do(func(tx *Tx) error {
				if transition == "result_pending" {
					return tx.ExecutionStatus(run, "succeeded", nil)
				}
				return tx.ExecutionStatus(run, "cancelled", json.RawMessage(`{"status":"failed","error":"writer stopped"}`))
			})
			checkBlocked(false)
		})
	}
}
