package board

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestExecutionRejectsRetiredLeaseNames(t *testing.T) {
	f := newPlanFixture(t)
	f.tx(func(tx *Tx) error {
		g, err := tx.Load("proj_001")
		if err != nil {
			return err
		}
		g.Intents = []Intent{{ID: "step", Worker: Ptr("worker@run")}}
		for _, lease := range []string{"bootstrap", "intent", "unknown", ""} {
			fence := ExecutionFence{Run: "worker@run", Lease: lease, Intent: "step"}
			requireAPIStatus(t, tx.CheckExecution(g, fence), 403)
		}
		return tx.CheckExecution(g, ExecutionFence{Run: "worker@run", Lease: "explore", Intent: "step"})
	})
}

func TestExecutionReadUsesRoleTimeoutWithoutExpiringStoredLeases(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "read-lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const heartbeat = "2026-01-01T00:00:00Z"
	graph := Graph{
		Project: Project{ID: "p", Title: "Lease fixture", Status: "active", CreatedAt: heartbeat, OrchestrationVersion: 1,
			Reason:  &Reason{Worker: "planner", Trigger: "plan", StartedAt: heartbeat, Heartbeat: heartbeat},
			Curator: &Reason{Worker: "curator", Trigger: "review", StartedAt: heartbeat, Heartbeat: heartbeat}},
		Facts: []Fact{}, Hints: []Hint{},
		Intents: []Intent{{ID: "step", Description: "Inspect fixture", Creator: "planner", CreatedAt: heartbeat,
			From: []string{}, Worker: Ptr("worker"), Heartbeat: Ptr(heartbeat)}},
	}
	if err := store.Do(context.Background(), func(tx *Tx) error {
		if err := tx.UpdateSettings(Settings{ReasonTimeout: 60, IntentTimeout: 15}); err != nil {
			return err
		}
		return tx.Save(graph)
	}); err != nil {
		t.Fatal(err)
	}
	for _, seconds := range []int{30, 70} {
		store.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, seconds, 0, time.UTC) }
		if err := store.View(context.Background(), func(tx *Tx) error {
			loaded, err := tx.Load("p")
			if err != nil {
				return err
			}
			for _, fence := range []ExecutionFence{
				{Run: "planner", Lease: "reason"}, {Run: "curator", Lease: "curate"}, {Run: "worker", Lease: "explore", Intent: "step"},
			} {
				err := tx.CheckExecutionRead(loaded, fence)
				if fence.Lease == "explore" || seconds == 70 {
					requireAPIStatus(t, err, 409)
					if err.Error() != "Execution lease has expired" {
						t.Fatalf("wrong expiry error: %v", err)
					}
				} else if err != nil {
					t.Fatalf("control lease used the intent timeout: %v", err)
				}
			}
			after, err := tx.Load("p")
			if err != nil || !reflect.DeepEqual(after, graph) {
				t.Fatalf("read expiry changed stored leases: %+v, %v", after, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
