package board

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/board/internal/sqlstore"
)

type stateBenchmarkSize struct {
	intents, descriptionBytes, sources int
}

type stateBenchmarkFixture struct {
	store   *Store
	size    stateBenchmarkSize
	version string
	from    []string
}

const stateBenchmarkProject = "benchmark"

var (
	stateBenchmarkPlanner = ExecutionFence{Run: "planner@benchmark", Lease: "reason"}
	stateBenchmarkWorker  = ExecutionFence{Run: "worker@benchmark-worker", Lease: "explore", Intent: "active"}
)

// Share persisted graph fixtures between regressions and benchmarks. Direct
// planner domain mutations remain unregistered; executions use current protocols.
func newStateBenchmarkFixture(tb testing.TB, size stateBenchmarkSize, registeredDecision bool) *stateBenchmarkFixture {
	tb.Helper()
	store, err := Open(filepath.Join(tb.TempDir(), "state.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = store.Close() })
	store.Now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	f := &stateBenchmarkFixture{store: store, size: size}
	err = store.Do(context.Background(), func(tx *Tx) error {
		g := Graph{
			Project: Project{ID: stateBenchmarkProject, Title: "Synthetic benchmark", Status: "active", CreatedAt: tx.Now, OrchestrationVersion: 1,
				Reason: &Reason{Worker: stateBenchmarkPlanner.Run, Trigger: "initial", StartedAt: tx.Now, Heartbeat: tx.Now}},
			Facts:   []Fact{{ID: "origin", Description: "Inspect the synthetic fixture"}, {ID: "goal", Description: "Explain the synthetic fixture"}},
			Intents: []Intent{}, Hints: []Hint{},
		}
		for n := 0; n < size.sources; n++ {
			id := fmt.Sprintf("source%03d", n)
			f.from = append(f.from, id)
			g.Facts = append(g.Facts, Fact{ID: id, Description: "Independent synthetic observation " + id})
		}
		steps := []stepMetadata{}
		for n := 1; n <= size.intents; n++ {
			id, result := fmt.Sprintf("i%03d", n), fmt.Sprintf("f%03d", n)
			description := fmt.Sprintf("Historical task %04d: ", n) + strings.Repeat("x", size.descriptionBytes-22)
			g.Facts = append(g.Facts, Fact{ID: result, Description: "Synthetic result " + result})
			g.Intents = append(g.Intents, Intent{ID: id, From: f.from, To: Ptr(result), Description: description,
				Creator: "history", Worker: Ptr("worker@history"), CreatedAt: tx.Now, ConcludedAt: Ptr(tx.Now)})
			steps = append(steps, stepMetadata{ID: id, GoalID: "goal"})
		}
		g.Intents = append(g.Intents, Intent{ID: "active", From: []string{"origin"}, Description: "Observe fixture",
			Creator: "history", Worker: Ptr(stateBenchmarkWorker.Run), Heartbeat: Ptr(tx.Now), CreatedAt: tx.Now})
		if err := tx.Save(g); err != nil {
			return err
		}
		for _, kind := range []string{"fact", "intent"} {
			if _, err := tx.transactionQueries.Exec("INSERT INTO scoped_counters(project_id,kind,value) VALUES(?,?,?)", stateBenchmarkProject, kind, size.intents); err != nil {
				return err
			}
		}
		facts := []FactRecord{}
		for _, fact := range g.Facts {
			if fact.ID == "origin" || fact.ID == "goal" {
				continue
			}
			record := FactRecord{ID: fact.ID, Description: fact.Description, Scope: "fixture", ObservedAt: tx.Now, Status: "valid", RunID: "worker@history", Evidence: []EvidenceRef{{RunID: "worker@history", Path: "evidence/" + fact.ID, Excerpt: fact.Description}}}
			if strings.HasPrefix(fact.ID, "f") {
				record.SourceStepID = "i" + fact.ID[1:]
				record.RunID = "worker@history-" + fact.ID[1:]
				record.Evidence[0].RunID = record.RunID
			}
			facts = append(facts, record)
		}
		raw, err := json.Marshal(map[string]any{"goals": []Goal{}, "steps": steps, "facts": facts, "findings": []Finding{}, "fact_relations": []FactRelation{}})
		if err != nil {
			return err
		}
		if err = sqlstore.SaveStateMetadata(tx.transactionQueries, stateBenchmarkProject, string(raw), 0, 0); err != nil {
			return err
		}
		// Historical bodies may be archived; support projection uses their durable identities.
		for n := 1; n <= size.intents; n++ {
			step, run := fmt.Sprintf("i%03d", n), fmt.Sprintf("history-%03d", n)
			e := Execution{ProjectID: stateBenchmarkProject, ID: run, Namespace: "benchmark", Backend: "worker", Kind: "explore", Intent: step, Lease: "worker@" + run, RetryKey: "explore:" + step, Status: "succeeded", Job: json.RawMessage(`{}`), CreatedAt: tx.Now, UpdatedAt: tx.Now}
			if err := sqlstore.InsertExecution(tx.transactionQueries, e, ExecutionSummary{}, ""); err != nil {
				return err
			}
		}
		state, err := tx.State(stateBenchmarkProject)
		if err != nil {
			return err
		}
		f.version = DecisionStateVersion(state)
		if err := tx.RegisterExecution(Execution{ProjectID: stateBenchmarkProject, ID: "benchmark-worker", Namespace: "benchmark", Backend: "worker", Kind: "explore", Intent: "active", Lease: stateBenchmarkWorker.Run, RetryKey: "explore:active", Job: fixtureExecutionJob(tb, tx, stateBenchmarkProject, "explore", "")}); err != nil {
			return err
		}
		if !registeredDecision {
			return nil
		}
		job := fixtureExecutionJob(tb, tx, stateBenchmarkProject, "reason", "")
		return tx.RegisterExecution(Execution{ProjectID: stateBenchmarkProject, ID: "benchmark", Namespace: "benchmark", Backend: "planner",
			Kind: "reason", Lease: stateBenchmarkPlanner.Run, Job: job, RetryKey: "reason:benchmark"})
	})
	if err != nil {
		tb.Fatal(err)
	}
	return f
}

func (f *stateBenchmarkFixture) action(op string, index int) StateAction {
	payload := map[string]any{"action": "add", "from": f.from,
		"description": fmt.Sprintf("New task %02d: ", index) + strings.Repeat("y", f.size.descriptionBytes-13)}
	if op == "fact" {
		payload = map[string]any{"description": "New synthetic observation", "scope": "fixture", "observed_at": "2026-01-02T03:04:05Z",
			"evidence": []EvidenceRef{{RunID: stateBenchmarkWorker.Run, Path: "evidence/fixture.txt", Excerpt: "Synthetic observation"}}}
	}
	raw, _ := json.Marshal(payload)
	return StateAction{Op: op, IdempotencyKey: fmt.Sprintf("benchmark:%s:%d", op, index), Payload: raw, ExpectedVersion: f.version}
}

func (f *stateBenchmarkFixture) batch(count int) DecisionBatch {
	batch := DecisionBatch{ExpectedVersion: f.version}
	for n := 0; n < count; n++ {
		batch.Actions = append(batch.Actions, DecisionAction{Op: "step", Ref: fmt.Sprintf("step%d", n), Payload: f.action("step", n).Payload})
	}
	return batch
}
