package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

const missingRoot = `{"status":"missing","from":["f001"],"description":"The user requires verifier acceptance; the observation is a refusal.","gaps":[{"id":"acceptance","input_ids":["goal"],"description":"Obtain actual verifier acceptance"}]}`
const satisfiedRoot = `{"status":"satisfied","from":["f001","f002"],"description":"Both user-requested flags have direct response evidence"}`

func TestRootAssessmentPromptRequiresEvaluationBeforeCompletionReuse(t *testing.T) {
	job := draftRunJob(t)
	job.Decision.ClosureProtocol = 1
	job.Decision.CompletionAssessment = assessmentFixture(job.Decision.StateVersion)
	prompt, err := Prompt(job, false, "/run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "After the root assessment has been observed in a subsequent model request") || strings.Contains(prompt, "Commit in this response only") {
		t.Fatal("completion reuse instructions contradicted the mandatory root assessment turn")
	}
}

func TestRootAssessmentSeparatesEvaluationFromPlanningAndBindsNewWork(t *testing.T) {
	d := &decisionDraft{orchestration: true, closureProtocol: true}
	ctx := context.Background()
	step := draftTestAction("step", "verify", `{"action":"add","from":["f001"],"description":"Obtain verifier acceptance"}`)
	if _, err := d.action(ctx, step, "v1", "acceptance"); err == nil {
		t.Fatal("new work bypassed the original goal assessment")
	}
	if _, err := d.assessRoot(json.RawMessage(missingRoot), "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.action(ctx, step, "v1", "acceptance"); err == nil || len(d.actions) != 0 {
		t.Fatal("preplanned tool group bypassed the assessment turn boundary")
	}
	loop := &agent.Loop{}
	d.beforeRequest(loop)
	if len(loop.ContextData) != 1 || !strings.Contains(loop.ContextData[0], "verifier acceptance") {
		t.Fatal("assessment was not retained through context compaction")
	}
	for _, gap := range []string{"", "invented-submission"} {
		if _, err := d.action(ctx, step, "v1", gap); err == nil || len(d.actions) != 0 {
			t.Fatal("work without an assessed requirement gap was admitted")
		}
	}
	if _, err := d.action(ctx, step, "v1", "acceptance"); err != nil {
		t.Fatal(err)
	}
	d.request = func(_ context.Context, r GraphRequest) (string, error) {
		if r.Batch.Assessment == nil || r.Batch.Assessment.Status != "missing" || r.Batch.Actions[0].GapID != "acceptance" || r.Batch.ExpectedVersion != "v1" {
			t.Fatal("batch lost its root assessment, gap or version")
		}
		return `{"committed":true}`, nil
	}
	if _, err := d.action(ctx, board.StateAction{Op: "commit"}, "v2"); err != nil || !d.committed {
		t.Fatalf("valid missing requirement could not continue: %v", err)
	}
}

func TestSatisfiedRootCannotGrowPlansOrReuseAfterResetAndConflict(t *testing.T) {
	for _, invalidate := range []bool{false, true} {
		t.Run(map[bool]string{false: "reset", true: "state_changed"}[invalidate], func(t *testing.T) {
			d := &decisionDraft{orchestration: true, closureProtocol: true}
			ctx := context.Background()
			if _, err := d.assessRoot(json.RawMessage(satisfiedRoot), "v1"); err != nil {
				t.Fatal(err)
			}
			d.beforeRequest(&agent.Loop{})
			for _, a := range []board.StateAction{
				draftTestAction("step", "submit", `{"action":"add","from":["f001"],"description":"Find an unrequested platform and submit"}`),
				draftTestAction("goal", "sweep", `{"action":"add","condition":"Prove no third flag exists"}`),
				draftTestAction("curation_request", "review", `{"sources":["f001","f002"],"reason":"Recheck flag numbering"}`),
			} {
				if _, err := d.action(ctx, a, "v1", "extra"); err == nil {
					t.Fatal("satisfied root expanded work", a.Op)
				}
			}
			if _, err := d.action(ctx, draftTestAction("step", "retire", `{"action":"abandon","id":"i003","reason":"Optional exploration is unnecessary after both requested flags were observed"}`), "v1"); err != nil {
				t.Fatal(err)
			}
			if invalidate {
				d.invalidate()
				d.observeRead("overview")
				d.observeRead("facts")
			} else if _, err := d.action(ctx, board.StateAction{Op: "reset"}, "v1"); err != nil {
				t.Fatal(err)
			}
			d.beforeRequest(&agent.Loop{})
			if d.rootAssessment != nil || d.rootReady || len(d.actions) != 0 {
				t.Fatal("root assessment survived invalidation")
			}
			if _, err := d.action(ctx, draftTestAction("complete", "done", `{"from":["f001","f002"],"description":"Both flags"}`), "v2"); err == nil {
				t.Fatal("old satisfied verdict authorized completion after reset/conflict")
			}
		})
	}
}
