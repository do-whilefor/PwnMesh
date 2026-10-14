package sqlstore

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestReadLeaseExpiryPreservesSQLiteDateSemantics(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.View(context.Background(), func(q Queries) error {
		for _, test := range []struct {
			name, now, heartbeat string
			timeout              int
			expired              bool
		}{
			{"live", "2026-01-01T00:00:10Z", "2026-01-01T00:00:00Z", 15, false},
			{"expired", "2026-01-01T00:00:20Z", "2026-01-01T00:00:00Z", 15, true},
			{"equal", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", 0, false},
			{"sqlite_date", "2026-01-01 00:00:10", "2026-01-01 08:00:00+08:00", 15, false},
			{"sqlite_expired", "2026-01-01 00:00:20", "2026-01-01 08:00:00+08:00", 15, true},
			{"missing_heartbeat", "2026-01-01T00:00:00Z", "", 15, true},
			{"invalid_heartbeat", "2026-01-01T00:00:00Z", "invalid", 15, true},
			{"invalid_now", "invalid", "2026-01-01T00:00:00Z", 15, true},
		} {
			expired, err := ReadLeaseExpired(q, test.now, test.heartbeat, test.timeout)
			if err != nil || expired != test.expired {
				t.Errorf("%s expiry = %v, want %v: %v", test.name, expired, test.expired, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func leaseGraph(project string, version int) model.Graph {
	const old = "2026-09-28T08:00:00Z"
	return model.Graph{
		Project: model.Project{ID: project, Title: "Lease fixture", Status: "active", CreatedAt: old, OrchestrationVersion: version,
			Reason:  &model.Reason{Worker: "planner", Trigger: "plan", StartedAt: old, Heartbeat: old},
			Curator: &model.Reason{Worker: "curator", Trigger: "review", StartedAt: old, Heartbeat: old}},
		Facts: []model.Fact{{ID: "goal", Description: "Goal"}},
		Hints: []model.Hint{},
		Intents: []model.Intent{
			{ID: "open", Description: "Open task", Creator: "planner", CreatedAt: old, From: []string{}, Worker: model.Ptr("worker"), Heartbeat: model.Ptr(old)},
			{ID: "done", Description: "Completed task", Creator: "planner", CreatedAt: old, From: []string{}, Worker: model.Ptr("completed-worker"), Heartbeat: model.Ptr(old), To: model.Ptr("goal"), ConcludedAt: model.Ptr(old)},
		},
	}
}

func TestLeaseWritesKeepCallerTransactionAndProjectScope(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	before := leaseGraph("p", 1)
	other := leaseGraph("other", 1)
	if err := store.Do(ctx, func(q Queries) error {
		for _, graph := range []model.Graph{before, other} {
			if err := SaveGraph(q, graph); err != nil {
				return err
			}
			if err := InsertExecution(q, model.Execution{ProjectID: graph.Project.ID, ID: "saved", Lease: "registered-worker", Job: []byte(`{}`)}, model.ExecutionSummary{}, ""); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	write := func(q Queries) error {
		if err := RevokeProjectRuns(q, "p"); err != nil {
			return err
		}
		if err := SaveReasonLease(q, "p", &model.Reason{Worker: "next", Trigger: "next", StartedAt: "started", Heartbeat: "heartbeat"}); err != nil {
			return err
		}
		if err := SaveIntentLease(q, "p", "open", "next", "heartbeat"); err != nil {
			return err
		}
		if err := ReleaseCuratorLease(q, "p", "different"); err != nil {
			return err
		}
		graph, err := LoadGraph(q, "p")
		if err != nil {
			return err
		}
		if graph.Project.Curator == nil || graph.Project.Curator.Worker != "curator" || graph.Project.Reason.Worker != "next" || model.Value(graph.Intents[0].Worker) != "next" {
			t.Fatalf("lease write lost its identity or released another owner: %+v", graph)
		}
		if err := SaveReasonLease(q, "p", nil); err != nil {
			return err
		}
		if err := ReleaseIntentLease(q, "p", "open"); err != nil {
			return err
		}
		return ReleaseCuratorLease(q, "p", "curator")
	}
	rejected := errors.New("outer operation rejected")
	if err := store.Do(ctx, func(q Queries) error {
		if err := write(q); err != nil {
			return err
		}
		return rejected
	}); !errors.Is(err, rejected) {
		t.Fatalf("outer rejection = %v", err)
	}
	check := func(committed bool) {
		t.Helper()
		if err := store.View(ctx, func(q Queries) error {
			for _, want := range []model.Graph{leaseGraph("p", 1), other} {
				if committed && want.Project.ID == "p" {
					want.Project.Reason, want.Project.Curator = nil, nil
					want.Intents[0].Worker, want.Intents[0].Heartbeat = nil, model.Ptr("heartbeat")
				}
				got, err := LoadGraph(q, want.Project.ID)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("lease writes changed another field or ignored caller rollback: got %+v, want %+v, error %v", got, want, err)
				}
				for _, worker := range []string{"planner", "curator", "worker", "completed-worker", "registered-worker", "next"} {
					revoked, err := RunRevoked(q, want.Project.ID, worker)
					if err != nil || revoked != (committed && want.Project.ID == "p" && worker != "next") {
						t.Fatalf("revocation scope or rollback changed for %s/%s: %v, %v", want.Project.ID, worker, revoked, err)
					}
				}
			}
			if committed {
				var cleared bool
				err := q.QueryRow(`SELECT reason_worker IS NULL AND reason_trigger IS NULL AND reason_started_at IS NULL AND reason_last_heartbeat_at IS NULL
AND curator_worker IS NULL AND curator_trigger IS NULL AND curator_started_at IS NULL AND curator_heartbeat IS NULL
FROM projects JOIN pwnmesh_project_orchestration ON project_id=id WHERE id='p'`).Scan(&cleared)
				if err != nil || !cleared {
					t.Fatalf("release retained partial control lease columns: %v, %v", cleared, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	check(false)
	if err := store.Do(ctx, write); err != nil {
		t.Fatal(err)
	}
	check(true)
}

func TestLeaseExpiryUsesCallerPolicyAndRetainsUnsupportedRows(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	graphs := []model.Graph{leaseGraph("p", 1), leaseGraph("other", 1), leaseGraph("legacy", 0), leaseGraph("future", 2)}
	if err := store.Do(ctx, func(q Queries) error {
		for _, graph := range graphs {
			if err := SaveGraph(q, graph); err != nil {
				return err
			}
		}
		return ExpireLeases(q, "p", "2026-09-28T08:00:30Z", 1, 10, 60)
	}); err != nil {
		t.Fatal(err)
	}
	check := func(global bool) {
		t.Helper()
		if err := store.View(ctx, func(q Queries) error {
			for _, original := range graphs {
				want := leaseGraph(original.Project.ID, original.Project.OrchestrationVersion)
				if want.Project.OrchestrationVersion == 1 {
					if global || want.Project.ID == "p" {
						want.Intents[0].Worker = nil
					}
					if global {
						want.Project.Reason, want.Project.Curator = nil, nil
					}
				}
				got, err := LoadGraph(q, want.Project.ID)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("expiry changed scope, version, completed owner or retained heartbeat: got %+v, want %+v, error %v", got, want, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	check(false)
	if err := store.Do(ctx, func(q Queries) error {
		return ExpireLeases(q, "", "2026-09-28T08:02:00Z", 1, 10, 60)
	}); err != nil {
		t.Fatal(err)
	}
	check(true)
}
