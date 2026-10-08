package board

import (
	"reflect"
	"slices"
	"testing"
)

// Asset identity is a discovery relation. Only explicit Fact/Step edges carry
// premises, even when every observation and task concerns the same address.
func TestAssetDiscoveryDoesNotCreateSupportOrDependencyEdges(t *testing.T) {
	f := newOrchestrationFixture(t)
	const host = "shared.example.test"
	step := func(label string, from, dependencies []string) string {
		t.Helper()
		return f.action(f.planner, "step", "step:"+label, map[string]any{
			"action": "add", "description": label, "from": from, "depends_on": dependencies,
			"assets": []AssetSpec{{Kind: "host", Value: host}},
		}, "").ID
	}
	upstream := step("upstream", []string{"origin"}, nil)
	producer := f.worker("producer", upstream)
	result := assetFact(f, producer, "accepted-result", host)
	f.finish(producer, result)
	sibling := step("independent completed task", []string{"origin"}, nil)
	siblingRun := f.worker("sibling", sibling)
	siblingFact := assetFact(f, siblingRun, "independent-result", host)
	f.finish(siblingRun, siblingFact)

	fromStep := step("explicit fact consumer", []string{result}, nil)
	dependencyStep := step("explicit step consumer", []string{"origin"}, []string{upstream})
	independentStep := step("same asset without dependency", []string{"origin"}, nil)
	runs := make(map[string]Execution)
	for _, id := range []string{fromStep, dependencyStep, independentStep} {
		run := dependencyRun(f, "run-"+id, id)
		f.do(func(tx *Tx) error { return tx.RegisterExecution(run) })
		f.do(func(tx *Tx) error { return tx.ExecutionStatus(run, "running", nil) })
		runs[id] = run
	}
	before := f.state()
	var snapshot *InputSnapshot
	f.do(func(tx *Tx) (err error) { snapshot, err = tx.FreezeInput(before); return err })
	beforeHistory := before.History()
	if !slices.ContainsFunc(beforeHistory, func(entry HistoryEntry) bool { return entry.ID == sibling }) ||
		slices.ContainsFunc(beforeHistory, func(entry HistoryEntry) bool { return entry.ID == upstream }) {
		t.Fatal("asset identity either kept unrelated history hot or folded live dependency support")
	}

	clue := assetFact(f, runs[independentStep], "new-discovery", host)
	afterClue := f.state()
	if len(afterClue.Assets) != 1 || len(afterClue.Findings)+len(afterClue.Candidates)+len(afterClue.FactRelations) != 0 {
		t.Fatal("shared address merged observations or promoted discovery into a judgment")
	}
	for _, id := range []string{fromStep, dependencyStep, independentStep} {
		if !reflect.DeepEqual(dependencyStepState(t, before, id), dependencyStepState(t, afterClue, id)) {
			t.Fatal("a new same-asset clue rewrote a Step's authorized contract")
		}
		f.do(func(tx *Tx) error { return tx.CheckExecutionDependencies(runs[id]) })
	}
	if !reflect.DeepEqual(beforeHistory, afterClue.History()) {
		t.Fatal("same-asset discovery changed unrelated cold history")
	}

	correctionStep := step("independent correction", []string{"origin"}, nil)
	correctionRun := f.worker("correction", correctionStep)
	correction := assetFact(f, correctionRun, "corrected-result", host)
	f.finish(correctionRun, correction)
	curator, input := f.curator("correct")
	f.action(curator.Fence(), "curate", "correct-result", CuratePayload{
		ThroughRevision: input.Revision, Groups: []CurateGroup{},
		Relations: []CurateRelation{{Kind: "refutes", Source: correction, Target: result, Reason: "An explicit correction of the accepted premise"}},
	}, DecisionStateVersion(input))
	f.finish(curator, "")
	afterCorrection := f.state()
	if err := afterCorrection.ValidateFactSources([]string{siblingFact, clue}, true); err != nil {
		t.Fatalf("same-asset independent observations lost support: %v", err)
	}
	if err := afterCorrection.ValidateFactSources([]string{result}, true); err == nil {
		t.Fatal("explicitly refuted premise remained usable")
	}
	if got := dependencyStepState(t, afterCorrection, fromStep); !slices.Equal(got.InvalidSources, []string{result}) {
		t.Fatalf("Fact dependency did not retain its invalidation: %+v", got)
	}
	if got := dependencyStepState(t, afterCorrection, dependencyStep); !slices.Equal(got.BlockedBy, []string{upstream}) {
		t.Fatalf("Step dependency did not retain its invalidation: %+v", got)
	}
	if !reflect.DeepEqual(dependencyStepState(t, before, independentStep), dependencyStepState(t, afterCorrection, independentStep)) ||
		!dependencyStepState(t, afterCorrection, sibling).SupportValid {
		t.Fatal("explicit correction propagated through asset anchors to unrelated tasks")
	}
	f.do(func(tx *Tx) error {
		requireAPIStatus(t, tx.CheckExecutionDependencies(runs[fromStep]), 409)
		requireAPIStatus(t, tx.CheckExecutionDependencies(runs[dependencyStep]), 409)
		return tx.CheckExecutionDependencies(runs[independentStep])
	})
	for _, entry := range afterCorrection.History() {
		if entry.ID == upstream || entry.ID == correctionStep {
			t.Fatal("the explicit correction chain was folded into cold history")
		}
	}
	if !slices.ContainsFunc(afterCorrection.History(), func(entry HistoryEntry) bool { return entry.ID == sibling }) {
		t.Fatal("asset equality pulled unrelated completed work into the correction chain")
	}
	for _, anchor := range before.AssetAnchors {
		if !slices.Contains(afterCorrection.AssetAnchors, anchor) {
			t.Fatal("correction erased original asset provenance")
		}
	}

	// Restarting the store must preserve live invalidation while the registered
	// historical view retains its original evidence and discovery boundary.
	reopenAssetFixture(t, f)
	if !reflect.DeepEqual(afterCorrection, f.state()) {
		t.Fatal("reopening changed the live dependency/asset projection")
	}
	checkRecoveredAssetSnapshot(t, f, snapshot, before)
	f.do(func(tx *Tx) error {
		frozen, err := tx.ReadInputSnapshot("p", snapshot.ID)
		if err != nil {
			return err
		}
		if err := frozen.ValidateFactSources([]string{result}, true); err != nil {
			t.Fatalf("live correction leaked into frozen evidence: %v", err)
		}
		if !reflect.DeepEqual(beforeHistory, frozen.History()) {
			t.Fatal("live discovery/correction changed the frozen history view")
		}
		return nil
	})
}
