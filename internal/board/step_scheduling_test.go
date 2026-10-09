package board

import (
	"slices"
	"testing"
)

func TestScheduleStepsPreserveLegacyFencesAndStepAuthorization(t *testing.T) {
	intents := []Intent{
		{ID: "ready", CreatedAt: "first"}, {ID: "retry", CreatedAt: "second"},
		{ID: "ended", ConcludedAt: Ptr("now"), Worker: Ptr("old-run")}, {ID: "produced", To: Ptr("fact"), Worker: Ptr("old-run")},
		{ID: "leased", Worker: Ptr("run")}, {ID: "abandoned"}, {ID: "invalid"},
		{ID: "waiting"}, {ID: "completed"}, {ID: "review"}, {ID: "unknown"}, {ID: "missing"},
		{ID: "boot", Description: "bootstrap", Creator: "dispatcher.bootstrap", From: []string{"origin"}},
	}
	steps := []Step{
		{ID: "retry", Status: "failed", Priority: 3}, {ID: "ready", Status: "open", CreatedAt: "wrong"},
		{ID: "ended", Status: "open"}, {ID: "produced", Status: "open"}, {ID: "leased", Status: "open"},
		{ID: "abandoned", Status: "abandoned"}, {ID: "invalid", Status: "open", InvalidSources: []string{"f"}},
		{ID: "waiting", Status: "open", BlockedBy: []string{"upstream"}}, {ID: "completed", Status: "completed"},
		{ID: "review", Status: "needs_review"}, {ID: "unknown"}, {ID: "orphan", Status: "open"},
		{ID: "boot", Status: "open"},
	}
	projected := scheduleSteps(intents, steps)
	got := slices.DeleteFunc(slices.Clone(projected), func(step ScheduleStep) bool { return !step.Ready })
	if len(got) != 2 || got[0].ID != "ready" || got[0].CreatedAt != "first" || got[1].ID != "retry" || got[1].Priority != 3 {
		t.Fatalf("lost scheduling order, priority or compatibility fences: %+v", got)
	}
	if steps[1].CreatedAt != "wrong" {
		t.Fatal("scheduling changed the supplied Step")
	}
	if len(projected) != len(intents) || projected[11].ID != "missing" {
		t.Fatalf("projection lost persisted task order: %+v", projected)
	}
	for _, step := range projected {
		if step.Running != (step.ID == "leased") {
			t.Fatalf("incorrect active lease projection: %+v", step)
		}
	}
	s := executionQueryStore(t)
	executionQueryTx(t, s, func(tx *Tx) {
		checks, err := tx.ScheduleExecutionChecks("p", "ns", projected)
		if err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(checks))
		for key := range checks {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"explore:ready", "explore:retry"}) {
			t.Fatalf("registry checks and Step candidates disagree: %v", keys)
		}
	})
}
