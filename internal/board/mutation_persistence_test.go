package board

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestStateFactIDCollisionRollsBackWithoutSplittingObservation(t *testing.T) {
	f := newOrchestrationFixture(t)
	e := f.worker("observation", "")
	f.do(func(tx *Tx) error {
		g, err := tx.Load("p")
		if err != nil {
			return err
		}
		// A compatible graph writer may supply an ID after this database was
		// opened, so startup counter repair has not seen it yet.
		g.Facts = append(g.Facts, Fact{ID: "f001", Description: "Existing immutable observation"})
		return tx.Save(g)
	})
	before := f.state()
	payload, _ := json.Marshal(map[string]any{"description": "Different new observation", "scope": "fixture", "observed_at": "2026-09-28T08:00:00Z", "evidence": []EvidenceRef{{RunID: e.Lease, Path: "evidence/raw.txt", Excerpt: "new observation"}}})
	var beforeMetadata string
	var beforeActions, beforeEvents int
	f.do(func(tx *Tx) error {
		return tx.QueryRow(`SELECT data,(SELECT COUNT(*) FROM xloom_state_actions),(SELECT COUNT(*) FROM xloom_state_events) FROM xloom_state WHERE project_id='p'`).Scan(&beforeMetadata, &beforeActions, &beforeEvents)
	})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", e.Fence(), StateAction{Op: "fact", IdempotencyKey: "new-observation", Payload: payload})
		return err
	})
	requireAPIStatus(t, err, 409)
	if !strings.Contains(err.Error(), "f001") || !strings.Contains(err.Error(), "counter") {
		t.Fatalf("collision has no useful diagnostic: %v", err)
	}
	if after := f.state(); !reflect.DeepEqual(before, after) {
		t.Fatal("collision changed the original observation, projection or revision")
	}
	f.do(func(tx *Tx) error {
		var metadata string
		var actions, events int
		if err := tx.QueryRow(`SELECT data,(SELECT COUNT(*) FROM xloom_state_actions),(SELECT COUNT(*) FROM xloom_state_events) FROM xloom_state WHERE project_id='p'`).Scan(&metadata, &actions, &events); err != nil {
			return err
		}
		if metadata != beforeMetadata || actions != beforeActions || events != beforeEvents {
			t.Fatal("collision committed metadata, an idempotency receipt or an event")
		}
		id, err := tx.Next("p", "fact")
		if err == nil && id != "f001" {
			t.Fatalf("collision consumed an allocation despite rollback: %s", id)
		}
		return err
	})
}

func TestStateNodeAddsWriteOnlyNewNode(t *testing.T) {
	for _, op := range []string{"fact", "step"} {
		t.Run(op, func(t *testing.T) {
			f := newStateBenchmarkFixture(t, stateBenchmarkSize{16, 128, 4}, 1)
			err := f.store.Do(context.Background(), func(tx *Tx) error {
				for _, statement := range []string{
					`CREATE TRIGGER guard_project BEFORE UPDATE ON projects BEGIN SELECT RAISE(ABORT,'unrelated project write'); END`,
					`CREATE TRIGGER guard_fact BEFORE INSERT ON facts WHEN NEW.id != 'f017' BEGIN SELECT RAISE(ABORT,'unrelated fact write'); END`,
					`CREATE TRIGGER guard_intent BEFORE INSERT ON intents WHEN NEW.id != 'i017' BEGIN SELECT RAISE(ABORT,'unrelated intent write'); END`,
					`CREATE TRIGGER guard_sources BEFORE INSERT ON intent_sources WHEN NEW.intent_id != 'i017' BEGIN SELECT RAISE(ABORT,'unrelated source write'); END`,
				} {
					if _, err := tx.Exec(statement); err != nil {
						return err
					}
				}
				fence, want := stateBenchmarkPlanner, "i017"
				if op == "fact" {
					fence, want = stateBenchmarkWorker, "f017"
				}
				result, err := tx.StateAction(stateBenchmarkProject, fence, f.action(op, 0))
				if err != nil {
					return err
				}
				if result.ID != want || result.Revision != 1 {
					t.Fatalf("unexpected addition: %+v", result)
				}
				state, err := tx.State(stateBenchmarkProject)
				if err != nil {
					return err
				}
				if op == "step" {
					step := state.Steps[len(state.Steps)-1]
					if step.ID != want || len(step.From) != 4 {
						t.Fatalf("sources were not saved: %+v", step)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStateNodeFailureRollsBackAllPersistence(t *testing.T) {
	for _, op := range []string{"fact", "step"} {
		t.Run(op, func(t *testing.T) {
			f := newStateBenchmarkFixture(t, stateBenchmarkSize{16, 128, 4}, 1)
			snapshot := func() string {
				t.Helper()
				var raw string
				if err := f.store.Do(context.Background(), func(tx *Tx) error {
					state, err := tx.State(stateBenchmarkProject)
					if err != nil {
						return err
					}
					var metadata string
					var facts, intents, sources, actions, events, factCounter, intentCounter int
					if err = tx.QueryRow(`SELECT data,(SELECT COUNT(*) FROM facts),(SELECT COUNT(*) FROM intents),(SELECT COUNT(*) FROM intent_sources),(SELECT COUNT(*) FROM xloom_state_actions),(SELECT COUNT(*) FROM xloom_state_events),(SELECT value FROM scoped_counters WHERE project_id=? AND kind='fact'),(SELECT value FROM scoped_counters WHERE project_id=? AND kind='intent') FROM xloom_state WHERE project_id=?`, stateBenchmarkProject, stateBenchmarkProject, stateBenchmarkProject).Scan(&metadata, &facts, &intents, &sources, &actions, &events, &factCounter, &intentCounter); err != nil {
						return err
					}
					encoded, err := json.Marshal(state)
					raw = fmt.Sprintf("%s|%s|%d,%d,%d,%d,%d,%d,%d", encoded, metadata, facts, intents, sources, actions, events, factCounter, intentCounter)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return raw
			}
			before := snapshot()
			if err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.Exec(`CREATE TRIGGER reject_node_event BEFORE INSERT ON xloom_state_events BEGIN SELECT RAISE(ABORT,'event unavailable'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			fence := stateBenchmarkPlanner
			if op == "fact" {
				fence = stateBenchmarkWorker
			}
			if err := f.store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction(stateBenchmarkProject, fence, f.action(op, 0))
				return err
			}); err == nil {
				t.Fatal("injected event failure succeeded")
			}
			if after := snapshot(); after != before {
				t.Fatal("failed node addition changed graph, metadata, counters, receipt or event")
			}
		})
	}
}

func TestAbandonWritesOnlyTargetIntentAndRevokesOwner(t *testing.T) {
	f := newPlanFixture(t)
	id := f.action("step", "add", stepInput("Observe fixture", []string{"origin"}, "goal", 0)).ID
	f.tx(func(tx *Tx) error {
		if _, err := tx.Exec("UPDATE intents SET worker='worker@abandon',last_heartbeat_at=? WHERE project_id='proj_001' AND id=?", tx.Now, id); err != nil {
			return err
		}
		for _, statement := range []string{
			`CREATE TRIGGER guard_abandon_project BEFORE UPDATE ON projects BEGIN SELECT RAISE(ABORT,'abandon rewrote project'); END`,
			`CREATE TRIGGER guard_abandon_facts BEFORE INSERT ON facts BEGIN SELECT RAISE(ABORT,'abandon rewrote facts'); END`,
			`CREATE TRIGGER guard_abandon_sources BEFORE INSERT ON intent_sources BEGIN SELECT RAISE(ABORT,'abandon rewrote sources'); END`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
		return nil
	})
	f.action("step", "abandon", map[string]any{"action": "abandon", "id": id, "reason": "direction replaced"})
	state := f.state()
	if state.Steps[0].Status != "abandoned" || state.Steps[0].Worker != nil || state.Graph.Intents[0].ConcludedAt == nil || state.Graph.Intents[0].Heartbeat == nil {
		t.Fatalf("abandon lost runtime semantics: %+v", state.Graph.Intents[0])
	}
	f.tx(func(tx *Tx) error {
		revoked, err := tx.RunRevoked("proj_001", "worker@abandon")
		if err == nil && !revoked {
			t.Fatal("abandoned owner was not revoked")
		}
		return err
	})
}
