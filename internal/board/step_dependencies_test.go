package board

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func dependentStep(f *orchestrationFixture, label string, dependencies ...string) string {
	f.t.Helper()
	return f.action(f.planner, "step", "step:"+label+":"+strings.Join(dependencies, ","), map[string]any{"action": "add", "description": label, "from": []string{"origin"}, "depends_on": dependencies}, "").ID
}

func dependencyRun(f *orchestrationFixture, label, step string) Execution {
	f.t.Helper()
	e := Execution{ProjectID: "p", ID: label, Namespace: "test", Backend: "worker", Kind: "explore", Intent: step, Lease: "worker@" + label, RetryKey: "explore:" + step}
	f.do(func(tx *Tx) error {
		g, err := tx.Load("p")
		if err != nil {
			return err
		}
		for n := range g.Intents {
			if g.Intents[n].ID == step {
				g.Intents[n].Worker = Ptr(e.Lease)
			}
		}
		if err = tx.Save(g); err != nil {
			return err
		}
		s, err := tx.State("p")
		if err != nil {
			return err
		}
		results, err := tx.StepDependencyResults("p", step)
		if err != nil {
			return err
		}
		e.Job, err = json.Marshal(map[string]any{"kind": e.Kind, "run_id": e.ID, "graph": s.Graph, "state": s, "graph_rpc": true, "result_contract_version": 2, "dependency_results": results})
		return err
	})
	return e
}

func dependencyStepState(t *testing.T, s State, id string) Step {
	t.Helper()
	for _, step := range s.Steps {
		if step.ID == id {
			return step
		}
	}
	t.Fatalf("missing Step %s", id)
	return Step{}
}

func TestStepDependenciesWaitForAcceptedSuccessfulResult(t *testing.T) {
	f := newOrchestrationFixture(t)
	upstream := f.step("upstream", "")
	downstream := dependentStep(f, "downstream", upstream)
	assertBlocked := func() {
		t.Helper()
		step := dependencyStepState(t, f.state(), downstream)
		if step.Status != "blocked" || !slices.Equal(step.BlockedBy, []string{upstream}) {
			t.Fatalf("dependency was runnable: %+v", step)
		}
		f.do(func(tx *Tx) error { requireAPIStatus(t, tx.StepReady("p", downstream), 409); return nil })
	}
	assertBlocked()
	e := f.worker("producer", upstream)
	fact := f.fact(e, "result")
	assertBlocked() // An intermediate observation does not satisfy success.
	f.do(func(tx *Tx) error { _, err := tx.ConcludeEvidenceStep("p", e.Fence(), fact, nil); return err })
	assertBlocked() // Nor does acceptance without the successful run receipt.
	f.finish(e, "")
	ready := dependencyStepState(t, f.state(), downstream)
	if ready.Status != "open" || len(ready.BlockedBy) != 0 {
		t.Fatalf("success did not unblock: %+v", ready)
	}
	f.do(func(tx *Tx) error {
		results, err := tx.StepDependencyResults("p", downstream)
		if err == nil && !slices.Equal(results, []DependencyResult{{StepID: upstream, FactID: fact, RunID: e.ID}}) {
			t.Fatalf("wrong immutable dependencies: %+v", results)
		}
		return err
	})
	run := dependencyRun(f, "consumer", downstream)
	f.do(func(tx *Tx) error { return tx.RegisterExecution(run) })
	f.do(func(tx *Tx) error { return tx.ExecutionStatus(run, "running", nil) })
	result := f.fact(run, "consumer-result")
	f.finish(run, result)
	if !dependencyStepState(t, f.state(), downstream).SupportValid {
		t.Fatal("completed supported child was not accepted")
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	if !dependencyStepState(t, f.state(), downstream).SupportValid {
		t.Fatal("reopen lost dependency support")
	}
}

func TestStepDependenciesRejectMalformedAndImmutableEdges(t *testing.T) {
	f := newOrchestrationFixture(t)
	upstream := f.step("first", "")
	for _, deps := range [][]string{{"missing"}, {"i002"}, {upstream, upstream}} {
		raw, _ := json.Marshal(map[string]any{"action": "add", "description": "bad edge", "from": []string{"origin"}, "depends_on": deps})
		err := f.store.Do(context.Background(), func(tx *Tx) error {
			_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "bad", Payload: raw})
			return err
		})
		requireAPIStatus(t, err, 422)
	}
	child := dependentStep(f, "same description", upstream)
	again := f.action(f.planner, "step", "same-inputs", map[string]any{"action": "add", "description": "same description", "from": []string{"origin"}, "depends_on": []string{upstream}}, "")
	if child != again.ID || !again.Unchanged {
		t.Fatal("same dependency plan was duplicated")
	}
	other := dependentStep(f, "same description")
	if other == child {
		t.Fatal("different dependency inputs were conflated")
	}
	raw, _ := json.Marshal(map[string]any{"action": "priority", "id": upstream, "reason": "cannot create a cycle", "depends_on": []string{child}})
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "cycle", Payload: raw})
		return err
	})
	requireAPIStatus(t, err, 422)
	legacy := newPlanFixture(t)
	err = legacy.store.Do(context.Background(), func(tx *Tx) error {
		s, err := tx.State("proj_001")
		if err != nil {
			return err
		}
		return validateStepDependencies(s, []string{upstream})
	})
	requireAPIStatus(t, err, 422)
}

func TestExecutionRejectsUnboundOrChangedDependencyResult(t *testing.T) {
	for _, mode := range []string{"missing", "fact", "run", "step", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("producer", "")
			fact := f.fact(producer, "result")
			f.finish(producer, fact)
			child := dependentStep(f, "consumer", producer.Intent)
			e := dependencyRun(f, "consumer", child)
			var job map[string]json.RawMessage
			_ = json.Unmarshal(e.Job, &job)
			results := []DependencyResult{{StepID: producer.Intent, FactID: fact, RunID: producer.ID}}
			switch mode {
			case "missing":
				results = nil
			case "fact":
				results[0].FactID = "different"
			case "run":
				results[0].RunID = "different"
			case "step":
				results[0].StepID = "different"
			case "duplicate":
				results = append(results, results[0])
			}
			job["dependency_results"], _ = json.Marshal(results)
			e.Job, _ = json.Marshal(job)
			err := f.store.Do(context.Background(), func(tx *Tx) error { return tx.RegisterExecution(e) })
			requireAPIStatus(t, err, 409)
			if !strings.Contains(err.Error(), "dependency_invalidated") {
				t.Fatalf("wrong rejection: %v", err)
			}
		})
	}
}

func invalidateDependencyFact(f *orchestrationFixture, target string) {
	f.t.Helper()
	correction := f.worker("correction", "")
	correctingFact := f.fact(correction, "correction")
	f.finish(correction, correctingFact)
	curator, state := f.curator("invalidate")
	f.action(curator.Fence(), "curate", "invalidate", CuratePayload{ThroughRevision: state.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{{Kind: "refutes", Source: correctingFact, Target: target, Reason: "Independent correction invalidates the accepted upstream premise"}}}, DecisionStateVersion(state))
	f.finish(curator, "")
}

func TestDependencyInvalidationRejectsStartResumeAndLateSuccess(t *testing.T) {
	for _, phase := range []string{"prepared", "running", "result_pending"} {
		t.Run(phase, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("producer", "")
			fact := f.fact(producer, "result")
			f.finish(producer, fact)
			child := dependentStep(f, "consumer", producer.Intent)
			e := dependencyRun(f, "consumer", child)
			f.do(func(tx *Tx) error { return tx.RegisterExecution(e) })
			if phase != "prepared" {
				f.do(func(tx *Tx) error { return tx.ExecutionStatus(e, "running", nil) })
			}
			observation := f.fact(e, "independent-retained-observation")
			if phase == "result_pending" {
				f.do(func(tx *Tx) error {
					return tx.ExecutionStatus(e, "result_pending", json.RawMessage(`{"status":"success"}`))
				})
			}
			invalidateDependencyFact(f, fact)
			for _, check := range []func(*Tx) error{
				func(tx *Tx) error { return tx.CheckExecutionDependencies(e) },
				func(tx *Tx) error { return tx.ResumeExecution(e) },
				func(tx *Tx) error { _, err := tx.ConcludeEvidenceStep("p", e.Fence(), observation, nil); return err },
			} {
				err := f.store.Do(context.Background(), check)
				requireAPIStatus(t, err, 409)
				if !strings.Contains(err.Error(), "dependency_invalidated") {
					t.Fatalf("wrong rejection: %v", err)
				}
			}
			if phase == "prepared" {
				requireAPIStatus(t, f.store.Do(context.Background(), func(tx *Tx) error { return tx.ExecutionStatus(e, "running", nil) }), 409)
			}
			if phase == "result_pending" {
				requireAPIStatus(t, f.store.Do(context.Background(), func(tx *Tx) error { return tx.ExecutionStatus(e, "succeeded", nil) }), 409)
			}
			if err := f.state().ValidateFactSources([]string{observation}, true); err != nil {
				t.Fatalf("independent observation was erased: %v", err)
			}
		})
	}
}

func TestCompletedDependencyInvalidationPropagatesWithoutRewritingHistory(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	fact := f.fact(producer, "producer-result")
	f.finish(producer, fact)
	child := dependentStep(f, "child", producer.Intent)
	e := dependencyRun(f, "child", child)
	f.do(func(tx *Tx) error { return tx.RegisterExecution(e) })
	independent := f.fact(e, "independent")
	result := f.fact(e, "child-result")
	f.finish(e, result)
	// An ordinary fact dependency on an accepted result must also carry the
	// upstream success support; changing edge syntax must not bypass it.
	grandchild := f.action(f.planner, "step", "grandchild", map[string]any{"action": "add", "from": []string{result}, "description": "result fact consumer"}, "").ID
	grandRun := f.worker("grandchild", grandchild)
	grandResult := f.fact(grandRun, "grandchild-result")
	f.finish(grandRun, grandResult)
	goal := f.action(f.planner, "goal", "dependent-goal", map[string]any{"action": "add", "condition": "Accepted dependent result"}, "").ID
	f.action(f.planner, "goal", "achieve-dependent-goal", map[string]any{"action": "achieve", "id": goal, "sources": []string{grandResult}, "reason": "Result accepted"}, "")
	candidate := f.worker("candidate", "")
	c := f.candidate(candidate, grandResult, "verified")
	cf := f.fact(candidate, "candidate-result")
	f.finish(candidate, cf)
	curator, state := f.curator("merge")
	f.curate(curator, state, CurateGroup{CandidateIDs: []string{c}, Status: "verified", Reason: "Accepted result supports the producer judgment"})
	f.finish(curator, "")
	waitingSource := f.action(f.planner, "step", "waiting-source", map[string]any{"action": "add", "from": []string{grandResult}, "description": "pending evidence consumer"}, "").ID
	waitingDependency := dependentStep(f, "pending task consumer", grandchild)
	state = f.state()
	if !state.Findings[0].SupportValid {
		t.Fatal("supported Finding was invalid before the premise changed")
	}
	for _, id := range []string{waitingSource, waitingDependency} {
		if step := dependencyStepState(t, state, id); step.Status != "open" || len(step.InvalidSources)+len(step.BlockedBy) != 0 {
			t.Fatalf("supported pending Step was blocked: %+v", step)
		}
	}
	invalidateDependencyFact(f, fact)
	state = f.state()
	for _, id := range []string{producer.Intent, child, grandchild} {
		step := dependencyStepState(t, state, id)
		if step.Status != "completed" || step.SupportValid {
			t.Fatalf("history rewritten or stale success accepted: %+v", step)
		}
	}
	for _, id := range []string{result, grandResult} {
		requireAPIStatus(t, state.ValidateFactSources([]string{id}, true), 409)
		for _, observation := range state.FactRecords {
			if observation.ID == id && (observation.Status != "valid" || !observation.SupportInvalid) {
				t.Fatal("derived support changed the raw observation status")
			}
		}
	}
	if err := state.ValidateFactSources([]string{independent}, true); err != nil {
		t.Fatalf("unrelated raw evidence invalidated: %v", err)
	}
	for _, current := range state.Goals {
		if current.ID == goal && (current.Status != "achieved" || current.SupportValid) {
			t.Fatal("goal support did not follow the invalid result")
		}
	}
	if len(state.Findings) != 1 || state.Findings[0].SupportValid {
		t.Fatal("Finding retained invalid support")
	}
	if step := dependencyStepState(t, state, waitingSource); step.Status != "needs_review" || !slices.Equal(step.InvalidSources, []string{grandResult}) {
		t.Fatalf("pending fact consumer retained invalid support: %+v", step)
	}
	if step := dependencyStepState(t, state, waitingDependency); step.Status != "blocked" || !slices.Equal(step.BlockedBy, []string{grandchild}) {
		t.Fatalf("pending task consumer retained invalid support: %+v", step)
	}
	f.do(func(tx *Tx) error {
		requireAPIStatus(t, tx.ValidateStateCompletion("p", []string{independent}), 409)
		return nil
	})
}

func TestDependencyBatchResolvesOnlyEarlierStepReferences(t *testing.T) {
	for _, invalid := range []string{"", "self", "forward", "goal"} {
		t.Run(invalid, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			s := f.state()
			e := Execution{ProjectID: "p", ID: "plan", Namespace: "test", Backend: "planner", Kind: "reason", Lease: f.planner.Run, RetryKey: "reason:batch"}
			e.Job, _ = json.Marshal(map[string]any{"kind": "reason", "run_id": e.ID, "graph": s.Graph, "state": s, "graph_rpc": true, "result_contract_version": 2, "decision": map[string]any{"version": 2, "state_version": DecisionStateVersion(s)}, "budget": map[string]int{"max_intents": 4}})
			f.do(func(tx *Tx) error { return tx.RegisterExecution(e) })
			first := json.RawMessage(`{"action":"add","from":["origin"],"description":"produce"}`)
			second := json.RawMessage(`{"action":"add","from":["origin"],"description":"consume","depends_on":["$first"]}`)
			actions := []DecisionAction{{Op: "step", Ref: "first", Payload: first}, {Op: "step", Ref: "second", Payload: second}}
			switch invalid {
			case "self":
				actions[0].Payload = json.RawMessage(`{"action":"add","from":["origin"],"description":"produce","depends_on":["$first"]}`)
			case "forward":
				actions[0].Payload = json.RawMessage(`{"action":"add","from":["origin"],"description":"produce","depends_on":["$second"]}`)
			case "goal":
				actions[0] = DecisionAction{Op: "goal", Ref: "first", Payload: json.RawMessage(`{"action":"add","condition":"child goal"}`)}
			}
			batch := DecisionBatch{ExpectedVersion: DecisionStateVersion(s), Actions: actions}
			var receipt DecisionReceipt
			err := f.store.Do(context.Background(), func(tx *Tx) error { var err error; receipt, err = tx.CommitDecision("p", f.planner, batch); return err })
			if invalid != "" {
				requireAPIStatus(t, err, 422)
				if current := f.state(); current.Revision != s.Revision || len(current.Steps) != 0 {
					t.Fatal("rejected dependency batch partially committed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			child := dependencyStepState(t, f.state(), receipt.IDs["second"])
			if child.Status != "blocked" || !slices.Equal(child.DependsOn, []string{receipt.IDs["first"]}) {
				t.Fatalf("prior reference not preserved: %+v", child)
			}
		})
	}
}

func TestDependencyFailureAndGenerationNeverUnblock(t *testing.T) {
	for _, terminal := range []string{"failed", "cancelled", "rejected"} {
		t.Run(terminal, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			producer := f.worker("producer", "")
			f.fact(producer, "partial-result")
			child := dependentStep(f, "child", producer.Intent)
			f.do(func(tx *Tx) error {
				return tx.ExecutionStatus(producer, terminal, json.RawMessage(`{"status":"failed"}`))
			})
			if step := dependencyStepState(t, f.state(), child); step.Status != "blocked" {
				t.Fatalf("failed upstream unblocked child: %+v", step)
			}
			f.do(func(tx *Tx) error {
				_, err := tx.RestartProject("p", nil)
				if err != nil {
					return err
				}
				s, err := tx.State("p")
				if err != nil {
					return err
				}
				requireAPIStatus(t, validateStepDependencies(s, []string{producer.Intent}), 422)
				return nil
			})
		})
	}
}

func TestOrchestratedConclusionCannotBorrowPreviousAttemptEvidence(t *testing.T) {
	f := newOrchestrationFixture(t)
	old := f.worker("old-attempt", "")
	oldFact := f.fact(old, "old-evidence")
	f.do(func(tx *Tx) error { return tx.ExecutionStatus(old, "failed", json.RawMessage(`{"status":"failed"}`)) })
	f.do(func(tx *Tx) error { return tx.ExecutionStatus(old, "retry_requested", nil) })
	next := dependencyRun(f, "new-attempt", old.Intent)
	var job map[string]any
	_ = json.Unmarshal(next.Job, &job)
	job["previous_run_id"] = old.ID
	next.Job, _ = json.Marshal(job)
	f.do(func(tx *Tx) error { return tx.RegisterExecution(next) })
	err := f.store.Do(context.Background(), func(tx *Tx) error { _, err := tx.ConcludeEvidenceStep("p", next.Fence(), oldFact, nil); return err })
	requireAPIStatus(t, err, 409)
	if !strings.Contains(err.Error(), "current execution") {
		t.Fatalf("wrong stale-result rejection: %v", err)
	}
	fresh := f.fact(next, "new-evidence")
	f.finish(next, fresh)
	if !dependencyStepState(t, f.state(), old.Intent).SupportValid {
		t.Fatal("current attempt's fresh result was rejected")
	}
}

func TestExecuteContextPrioritizesAcceptedDependencyEvidence(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	result := f.fact(producer, "important-upstream")
	f.finish(producer, result)
	child := dependentStep(f, "consumer", producer.Intent)
	s := f.state()
	// Place large irrelevant records before the required upstream result to
	// exercise the bounded projection's stable relevance ordering.
	unrelated := make([]FactRecord, 120)
	for n := range unrelated {
		unrelated[n] = FactRecord{ID: fmt.Sprint("unrelated", n), Description: strings.Repeat("unrelated evidence ", 50), Status: "valid", Scope: "other"}
	}
	s.FactRecords = append(unrelated, s.FactRecords...)
	raw, err := ContextView(s, child, 8000)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Facts []contextFact `json:"fact_records"`
	}
	if err = json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	for _, fact := range view.Facts {
		if fact.ID == result && !fact.DetailsOmitted && len(fact.Evidence) > 0 {
			return
		}
	}
	t.Fatalf("accepted dependency evidence was omitted from bounded Execute input: %s", raw)
}

func TestOrchestrationHumanReopenFeedbackRemainsOriginalInput(t *testing.T) {
	f := newOrchestrationFixture(t)
	f.do(func(tx *Tx) error {
		g, err := tx.Load("p")
		if err != nil {
			return err
		}
		g.Facts = append(g.Facts, Fact{ID: "feedback", Description: "Please also inspect the newly supplied condition"})
		g.Intents = append(g.Intents, Intent{ID: "feedback-step", From: []string{"origin"}, To: Ptr("feedback"), Description: "external_feedback", Creator: "human", Worker: Ptr("human"), CreatedAt: tx.Now, ConcludedAt: Ptr(tx.Now)})
		return tx.SaveUserInput(g, "reopen", "feedback", "", map[string]string{"description": "new condition"}, nil)
	})
	s := f.state()
	if err := s.ValidateFactSources([]string{"feedback"}, false); err != nil {
		t.Fatalf("human feedback became invalid execution evidence: %v", err)
	}
	f.action(f.planner, "step", "follow-feedback", map[string]any{"action": "add", "from": []string{"feedback"}, "description": "Follow the user's reopened condition"}, "")
	requireAPIStatus(t, validateStepDependencies(s, []string{"feedback-step"}), 422)
	// The synthetic historical Intent does not acquire Worker success authority.
	if dependencyStepState(t, s, "feedback-step").SupportValid {
		t.Fatal("human input became a succeeded Worker result")
	}
	raw := json.RawMessage(`{"action":"abandon","id":"feedback-step","reason":"Cannot retire the user's original feedback as failed execution"}`)
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: "retire-feedback", Payload: raw})
		return err
	})
	requireAPIStatus(t, err, 409)
}

func TestMainAgentCanRetireUnsupportedSuccessWithoutRewritingItsHistory(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("producer", "")
	result := f.fact(producer, "accepted-result")
	f.finish(producer, result)
	before := f.state()
	retire := func(key string) error {
		raw, _ := json.Marshal(map[string]any{"action": "abandon", "id": producer.Intent, "reason": "The accepted support was invalidated; fresh verification replaces this task"})
		return f.store.Do(context.Background(), func(tx *Tx) error {
			_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: key, Payload: raw})
			return err
		})
	}
	requireAPIStatus(t, retire("valid-success"), 409)
	invalidateDependencyFact(f, result)
	if err := retire("unsupported-success"); err != nil {
		t.Fatal(err)
	}
	s := f.state()
	step := dependencyStepState(t, s, producer.Intent)
	if step.Status != "abandoned" || step.Result == nil || *step.Result != result || step.SupportValid {
		t.Fatalf("retired result regained authority or disappeared: %+v", step)
	}
	for _, original := range before.Graph.Intents {
		if original.ID != producer.Intent {
			continue
		}
		for _, current := range s.Graph.Intents {
			if current.ID == producer.Intent && (Value(current.ConcludedAt) != Value(original.ConcludedAt) || Value(current.To) != Value(original.To) || Value(current.Worker) != Value(original.Worker)) {
				t.Fatal("retiring a current plan rewrote its historical success")
			}
		}
	}
	f.do(func(tx *Tx) error {
		e, err := tx.Execution("p", producer.ID)
		if err == nil && e.Status != "succeeded" {
			t.Fatal("successful execution was rewritten")
		}
		return err
	})
	replacement := f.worker("fresh-verification", "")
	fresh := f.fact(replacement, "fresh-result")
	f.finish(replacement, fresh)
	curator, input := f.curator("scan-replacement")
	f.curate(curator, input)
	f.finish(curator, "")
	f.do(func(tx *Tx) error {
		_, err := tx.CompleteProject("p", f.planner, []string{fresh}, "Fresh independent verification replaces the retired unsupported result")
		return err
	})
	if f.state().Graph.Project.Status != "completed" {
		t.Fatal("invalid historical support permanently prevented completion")
	}
}

func TestContextOmissionPreservesDerivedEvidenceInvalidity(t *testing.T) {
	s := State{Graph: Graph{Project: Project{ID: "p", OrchestrationVersion: 1}, Facts: []Fact{{ID: "origin", Description: "Input"}, {ID: "goal", Description: "Goal"}}}, Goals: []Goal{{ID: "goal", Status: "open"}}, FactRecords: []FactRecord{{ID: "stale-result", Description: strings.Repeat("long immutable observation ", 1000), Status: "valid", SupportInvalid: true}}}
	raw, err := ContextView(s, "", 4000)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Facts    []contextFact    `json:"fact_records"`
		Overview *contextOverview `json:"overview"`
	}
	if err = json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Facts) != 1 || !view.Facts[0].DetailsOmitted || !view.Facts[0].SupportInvalid {
		t.Fatalf("truncated fact lost support invalidity: %s", raw)
	}
	for _, section := range view.Overview.Sections {
		if section.Section == "facts" && (len(section.Items) != 1 || !section.Items[0].SupportInvalid) {
			t.Fatalf("overview presented stale evidence as usable: %s", raw)
		}
	}
}
