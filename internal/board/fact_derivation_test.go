package board

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func derivedFactPayload(f *orchestrationFixture, run Execution, label string, sources any) map[string]any {
	return map[string]any{"description": "Computed " + label, "scope": "controlled fixture", "observed_at": f.store.Now().Format(time.RFC3339),
		"evidence": []EvidenceRef{{RunID: run.Lease, Path: "/workspace/" + label + ".txt", Excerpt: "Retained calculation " + label}}, "derived_from": sources}
}

func TestFactDerivationPersistsAndInvalidatesTransitiveSupport(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	original := f.fact(producer, "original")
	f.finish(producer, original)
	calculator := f.worker("calculator", "")
	first := f.action(calculator.Fence(), "fact", "derived-first", derivedFactPayload(f, calculator, "first", []string{original}), "").ID
	second := f.action(calculator.Fence(), "fact", "derived-second", derivedFactPayload(f, calculator, "second", []string{first}), "").ID
	independent := f.fact(calculator, "independent")
	f.finish(calculator, independent)
	consumerID := f.action(f.planner, "step", "consumer", map[string]any{"action": "add", "from": []string{second}, "description": "Consume the calculated result"}, "").ID
	consumer := f.worker("consumer", consumerID)
	result := f.fact(consumer, "consumer-result")
	f.finish(consumer, result)
	terminal := f.worker("terminal", "")
	terminalFact := f.action(terminal.Fence(), "fact", "terminal-computation", derivedFactPayload(f, terminal, "terminal", []string{second}), "").ID
	f.finish(terminal, terminalFact)
	var snapshot *InputSnapshot
	before := f.state()
	for _, fact := range before.FactRecords {
		if fact.ID == first && !reflect.DeepEqual(fact.DerivedFrom, []string{original}) || fact.ID == second && !reflect.DeepEqual(fact.DerivedFrom, []string{first}) {
			t.Fatal("publication lost immutable derivation identities")
		}
	}
	f.do(func(tx *Tx) error { var err error; snapshot, err = tx.FreezeInput(before); return err })
	reopenAssetFixture(t, f)
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("reopening changed immutable derivation metadata")
	}
	corrector := f.worker("corrector", "")
	correction := f.fact(corrector, "correction")
	curator, current := f.curator("correct")
	f.action(curator.Fence(), "curate", "correct-source", CuratePayload{ThroughRevision: current.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{{Kind: "refutes", Source: correction, Target: original, Reason: "The retained original calculation used an incorrect input"}}}, DecisionStateVersion(current))
	after := f.state()
	for _, id := range []string{original, first, second, result, terminalFact} {
		if err := after.ValidateFactSources([]string{id}, true); err == nil {
			t.Fatalf("invalid derivation %s retained effective support", id)
		}
	}
	if err := after.ValidateFactSources([]string{independent}, true); err != nil {
		t.Fatalf("independent observation was invalidated: %v", err)
	}
	f.do(func(tx *Tx) error {
		changes, err := tx.StateChanges("p", current.Revision, after.Revision)
		if err != nil {
			return err
		}
		updates, err := BuildExecuteUpdates(after, ExecuteUpdateCursor{ProjectID: "p", Generation: after.Graph.Project.Generation, StepID: "consumer", RunID: "consumer", Revision: current.Revision}, []string{second}, changes, true, 0)
		if err == nil {
			found := map[string]bool{}
			for _, fact := range updates.Facts {
				found[fact.ID] = true
			}
			if !updates.Complete || !reflect.DeepEqual(updates.InvalidSources, []string{second}) || !found[first] || !found[original] || !found[correction] || found[independent] {
				t.Fatalf("correction update lost explicit derivation ancestry: %+v", updates)
			}
		}
		return err
	})
	f.do(func(tx *Tx) error {
		frozen, err := tx.ReadInputSnapshot("p", snapshot.ID)
		if err == nil && !reflect.DeepEqual(before, frozen) {
			t.Fatal("later correction rewrote the frozen derivation")
		}
		return err
	})
}

func TestFactDerivationRejectsInvalidatedFinalHandoff(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	original := f.fact(producer, "original")
	f.finish(producer, original)
	calculator := f.worker("calculator", "")
	derived := f.action(calculator.Fence(), "fact", "computed", derivedFactPayload(f, calculator, "computed", []string{original}), "").ID
	corrector := f.worker("corrector", "")
	correction := f.fact(corrector, "corrected")
	curator, state := f.curator("correct")
	f.action(curator.Fence(), "curate", "correct", CuratePayload{ThroughRevision: state.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{{Kind: "refutes", Source: correction, Target: original, Reason: "The previous input was incorrect"}}}, DecisionStateVersion(state))
	before := f.state()
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.ConcludeEvidenceStep("p", calculator.Fence(), derived, nil)
		return err
	})
	requireAPIStatus(t, err, 409)
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("invalidated result handoff changed shared state")
	}
	raw, _ := json.Marshal(derivedFactPayload(f, calculator, "stale", []string{derived}))
	err = f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", calculator.Fence(), StateAction{Op: "fact", IdempotencyKey: "stale-derivation", Payload: raw})
		return err
	})
	requireAPIStatus(t, err, 409)
}

func TestFactDerivationRejectsInvalidSourcesAtomically(t *testing.T) {
	f := newOrchestrationFixture(t)
	run := f.worker("producer", "")
	source := f.fact(run, "source")
	for _, sources := range []any{nil, "not-an-array", []string{""}, []string{"origin"}, []string{"missing"}, []string{source, source}, make([]string, 33)} {
		before := f.state()
		raw, _ := json.Marshal(derivedFactPayload(f, run, "invalid", sources))
		err := f.store.Do(context.Background(), func(tx *Tx) error {
			_, err := tx.StateAction("p", run.Fence(), StateAction{Op: "fact", IdempotencyKey: "invalid-derivation", Payload: raw})
			return err
		})
		if err == nil || !reflect.DeepEqual(before, f.state()) {
			t.Fatalf("invalid derivation changed shared state: %v; %v", sources, err)
		}
	}
}
