package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConcurrentObservationsKeepAtomicEventsAndReceipts(t *testing.T) {
	f := newOrchestrationFixture(t)
	const workers, writes = 6, 4
	runs := make([]Execution, workers)
	for i := range runs {
		runs[i] = f.worker(fmt.Sprintf("concurrent-%d", i), "")
	}
	second, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.Now = f.store.Now
	before := f.state()
	start := make(chan struct{})
	results := make(chan error, workers)
	for i, run := range runs {
		store := []*Store{f.store, second}[i%2]
		go func() {
			<-start
			for n := 0; n < writes; n++ {
				payload, _ := json.Marshal(map[string]any{"description": fmt.Sprintf("%s observation %d", run.ID, n), "scope": "concurrent fixture",
					"observed_at": "2026-09-28T08:00:00Z", "evidence": []EvidenceRef{{RunID: run.Lease, Path: "evidence.txt", Excerpt: "observed"}}})
				action := StateAction{Op: "fact", IdempotencyKey: fmt.Sprintf("%s:%d", run.ID, n), Payload: payload}
				aborted := errors.New("worker disconnected before commit")
				if err := store.Do(context.Background(), func(tx *Tx) error {
					if _, err := tx.StateAction("p", run.Fence(), action); err != nil {
						return err
					}
					return aborted
				}); !errors.Is(err, aborted) {
					results <- fmt.Errorf("rollback: %v", err)
					return
				}
				var committed StateActionResult
				for retry := 0; retry < 2; retry++ {
					if err := store.Do(context.Background(), func(tx *Tx) error {
						result, err := tx.StateAction("p", run.Fence(), action)
						if err != nil {
							return err
						}
						if retry != 0 && !reflect.DeepEqual(committed, result) {
							return fmt.Errorf("receipt changed after interleaved writers")
						}
						committed = result
						return nil
					}); err != nil {
						results <- err
						return
					}
				}
			}
			results <- nil
		}()
	}
	close(start)
	for range runs {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	after := f.state()
	if after.Revision != before.Revision+workers*writes || after.DecisionRevision != before.DecisionRevision+workers*writes || len(after.Graph.Facts) != len(before.Graph.Facts)+workers*writes {
		t.Fatalf("concurrent writes lost data or counted rolled-back work: revision=%d decision=%d facts=%d", after.Revision, after.DecisionRevision, len(after.Graph.Facts))
	}
	f.do(func(tx *Tx) error {
		events, err := tx.StateEvents("p", before.Revision)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for i, event := range events {
			if event.Revision != before.Revision+int64(i)+1 || event.Op != "fact" || seen[event.ID] {
				t.Fatalf("event gap, duplicate or uncommitted event: %+v", event)
			}
			seen[event.ID] = true
			var fact FactRecord
			if err := json.Unmarshal(event.Result, &fact); err != nil || fact.ID != event.ID || fact.RunID != event.RunID {
				t.Fatalf("event provenance diverged from its fact: %+v", event)
			}
		}
		if len(seen) != workers*writes {
			t.Fatalf("missing events: %d", len(seen))
		}
		return nil
	})
}

func TestActionIdempotencyDoesNotRoundEvidenceLineNumbers(t *testing.T) {
	f := newOrchestrationFixture(t)
	run := f.worker("precision", "")
	payload := json.RawMessage(`{"description":"Observed response","scope":"fixture","observed_at":"2026-09-28T08:00:00Z","evidence":[{"run_id":"worker@precision","path":"response.txt","start_line":9007199254740992,"end_line":9007199254740992,"excerpt":"response"}]}`)
	f.do(func(tx *Tx) error {
		_, err := tx.StateAction("p", run.Fence(), StateAction{Op: "fact", IdempotencyKey: "line-precision", Payload: payload})
		return err
	})
	changed := json.RawMessage(strings.Replace(string(payload), `"end_line":9007199254740992`, `"end_line":9007199254740993`, 1))
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", run.Fence(), StateAction{Op: "fact", IdempotencyKey: "line-precision", Payload: changed})
		return err
	})
	requireAPIStatus(t, err, 409)
}

func TestCanceledObservationTransactionCanRetryCleanly(t *testing.T) {
	f := newOrchestrationFixture(t)
	run := f.worker("disconnected", "")
	before := f.state()
	payload := json.RawMessage(`{"description":"Observed response","scope":"fixture","observed_at":"2026-09-28T08:00:00Z","evidence":[{"run_id":"worker@disconnected","path":"response.txt","excerpt":"response"}]}`)
	action := StateAction{Op: "fact", IdempotencyKey: "disconnected-write", Payload: payload}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := f.store.Do(ctx, func(tx *Tx) error {
		if _, err := tx.StateAction("p", run.Fence(), action); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if err == nil {
		t.Fatal("canceled transaction committed an observation")
	}
	if after := f.state(); !reflect.DeepEqual(before, after) {
		t.Fatal("canceled request left graph or revision changes behind")
	}
	f.do(func(tx *Tx) error {
		events, err := tx.StateEvents("p", before.Revision)
		if err != nil {
			return err
		}
		if len(events) != 0 {
			t.Fatal("canceled request published events")
		}
		result, err := tx.StateAction("p", run.Fence(), action)
		if err == nil && result.Revision != before.Revision+1 {
			t.Fatalf("retry retained the canceled revision: %+v", result)
		}
		return err
	})
}

func TestProjectExpiryDoesNotTouchOtherProjectsOrCompletedIntents(t *testing.T) {
	f := newOrchestrationFixture(t)
	f.do(func(tx *Tx) error {
		for _, id := range []string{"a", "b"} {
			old := time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC).Format(time.RFC3339)
			lease := &Reason{Worker: "stale", Trigger: "test", StartedAt: old, Heartbeat: old}
			g := Graph{Project: Project{ID: id, Title: id, Status: "active", CreatedAt: old, OrchestrationVersion: 1, Reason: lease, Curator: lease},
				Facts: []Fact{{ID: "origin", Description: "Input"}, {ID: "goal", Description: "Goal"}},
				Intents: []Intent{{ID: "pending", Description: "Pending", Creator: "worker", Worker: Ptr("stale"), Heartbeat: &old, CreatedAt: old},
					{ID: "completed", Description: "Completed", Creator: "worker", Worker: Ptr("stale"), Heartbeat: &old, CreatedAt: old, To: Ptr("goal"), ConcludedAt: &old}}}
			if err := tx.Save(g); err != nil {
				return err
			}
		}
		return nil
	})
	f.do(func(tx *Tx) error { return tx.ExpireProject("a") })
	check := func(project string, expired bool) {
		f.do(func(tx *Tx) error {
			g, err := tx.Load(project)
			if err != nil {
				return err
			}
			if (g.Project.Reason == nil) != expired || (g.Project.Curator == nil) != expired {
				t.Fatalf("%s control leases expired=%v: %+v", project, expired, g.Project)
			}
			for _, intent := range g.Intents {
				if (intent.Worker == nil) != (expired && intent.ID == "pending") {
					t.Fatalf("%s intent lease changed incorrectly: %+v", project, intent)
				}
			}
			return nil
		})
	}
	check("a", true)
	check("b", false)
	f.do(func(tx *Tx) error { return tx.ExpireProject("") })
	check("b", true)
}
