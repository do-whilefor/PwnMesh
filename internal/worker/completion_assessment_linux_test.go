//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func assessmentFixture(version string) *board.CompletionReview {
	return &board.CompletionReview{StateVersion: version, Acceptance: "not_checked",
		UserInputs: []board.Fact{{ID: "origin", Description: "Inspect production as anonymous"}, {ID: "goal", Description: "Obtain an actual response from /c"}, {ID: "feedback", Description: "Reopened requirement: retain the exact response status from /c"}},
		Hints:      []board.Hint{{ID: "hint", Creator: "user", Content: "Do not substitute a different environment"}}, From: []string{"f001"},
		FactRecords: []board.FactRecord{{ID: "f001", Description: "The response was received", Status: "valid", Scope: "production anonymous", Evidence: []board.EvidenceRef{{RunID: "producer", Path: "/retained/response.json", Excerpt: `{"response_received":true,"status":204}`}}}},
	}
}

func assessmentPreview(a *board.CompletionReview, payload json.RawMessage) board.DecisionReceipt {
	raw, _ := json.Marshal(a)
	var review board.CompletionReview
	_ = json.Unmarshal(raw, &review)
	var proposal struct {
		From        []string
		Description string
	}
	_ = json.Unmarshal(payload, &proposal)
	review.From, review.Description = proposal.From, proposal.Description
	return board.DecisionReceipt{ValidationScope: "protocol_only", CompletionReview: &review}
}

func assessmentObservedLoop(a *board.CompletionReview) *agent.Loop {
	raw, _ := json.Marshal(a)
	prompt := "<completion_assessment>\n" + string(raw) + "\n</completion_assessment>"
	return &agent.Loop{TaskPrompt: prompt, History: []agent.Message{agent.Text("user", prompt)}}
}

func assessmentProviderLoop(d *decisionDraft, loop *agent.Loop, p agent.Provider) *agent.Loop {
	loop.Provider, loop.ContextBytes, loop.StopResult = p, 1<<20, d.result
	loop.BeforeRequest = func(ctx context.Context, loop *agent.Loop) (context.Context, error) {
		d.beforeRequest(loop)
		return ctx, nil
	}
	loop.Tools = []agent.Tool{{Definition: agent.Definition{Name: "graph_action", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, payload json.RawMessage) (string, error) {
		var action board.StateAction
		if err := json.Unmarshal(payload, &action); err != nil {
			return "", err
		}
		return d.action(ctx, action, "v1")
	}}}
	return loop
}

func TestCompletionAssessmentMissingFromProviderCannotAuthorizeCommit(t *testing.T) {
	for _, promptOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing_packet", true: "task_without_history"}[promptOnly], func(t *testing.T) {
			d := &decisionDraft{orchestration: true, assessment: assessmentFixture("v1"), request: func(context.Context, GraphRequest) (string, error) {
				t.Fatal("unobserved assessment reached preview or commit")
				return "", nil
			}}
			loop := assessmentObservedLoop(d.assessment)
			loop.History = []agent.Message{agent.Text("user", "Continue the original task.")}
			if !promptOnly {
				loop.TaskPrompt = loop.History[0].Text()
			}
			calls := 0
			assessmentProviderLoop(d, loop, scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				for _, message := range history {
					if strings.Contains(message.Text(), "completion_assessment") {
						t.Fatal("fixture unexpectedly delivered its missing assessment")
					}
				}
				if d.assessmentReady || len(loop.ContextData) != 0 {
					t.Fatal("unsent assessment authorized completion")
				}
				if calls == 1 {
					m := draftModelCall("finish", "graph_action", `{"op":"complete","idempotency_key":"finish","payload":{"from":["f001"],"description":"Proof"}}`)
					m.Content = append(m.Content, draftModelCall("commit", "graph_action", `{"op":"commit"}`).Content...)
					return m, nil
				}
				results := history[len(history)-1].Content
				if calls != 2 || len(results) != 2 || results[0].IsError || !results[1].IsError {
					t.Fatalf("unseen completion was not rejected: %+v", results)
				}
				return agent.Text("assistant", "Explicit preview is required."), nil
			}))
			if _, err := loop.Run(context.Background(), ""); err != nil || calls != 2 || d.committed {
				t.Fatalf("missing assessment flow: err=%v calls=%d committed=%v", err, calls, d.committed)
			}
		})
	}
}

func TestCompletionAssessmentHiddenMismatchRequiresExplicitPreviewAndLaterRequest(t *testing.T) {
	a := assessmentFixture("v1")
	calls, previews, commits := 0, 0, 0
	const changed = "The authoritative preview includes a changed requirement"
	d := &decisionDraft{orchestration: true, assessment: a, request: func(_ context.Context, request GraphRequest) (string, error) {
		if request.Op == "decision_commit" {
			commits++
			if calls != 3 || previews != 2 {
				t.Fatal("hidden preview authorized commit before explicit later review")
			}
			return `{"committed":true}`, nil
		}
		if request.Op != "decision_preview" {
			t.Fatal("unexpected graph operation", request.Op)
		}
		previews++
		receipt := assessmentPreview(a, request.Batch.Actions[0].Payload)
		receipt.CompletionReview.UserInputs[1].Description = changed
		raw, err := json.Marshal(receipt)
		return string(raw), err
	}}
	loop := assessmentProviderLoop(d, assessmentObservedLoop(a), scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		raw, _ := json.Marshal(history)
		switch calls {
		case 1:
			if !d.assessmentReady {
				t.Fatal("actual first request did not authorize its supplied assessment")
			}
			m := draftModelCall("finish", "graph_action", `{"op":"complete","idempotency_key":"finish","payload":{"from":["f001"],"description":"Proof"}}`)
			m.Content = append(m.Content, draftModelCall("commit", "graph_action", `{"op":"commit"}`).Content...)
			return m, nil
		case 2:
			if d.reviewReady || d.reviewData != "" || strings.Contains(string(raw), changed) {
				t.Fatal("hidden mismatch result survived as observed evidence")
			}
			m := draftModelCall("premature", "graph_action", `{"op":"commit"}`)
			m.Content = append(m.Content, draftModelCall("explicit", "graph_action", `{"op":"preview"}`).Content...)
			return m, nil
		case 3:
			results := history[len(history)-1].Content
			if !d.reviewReady || !strings.Contains(string(raw), changed) || len(results) != 2 || !results[0].IsError || results[1].IsError {
				t.Fatalf("explicit review boundary was not enforced: %+v", results)
			}
			return draftModelCall("reviewed", "graph_action", `{"op":"commit"}`), nil
		default:
			t.Fatal("unexpected extra model request")
			return agent.Message{}, nil
		}
	}))
	if _, err := loop.Run(context.Background(), ""); err != nil || calls != 3 || previews != 2 || commits != 1 || !d.committed {
		t.Fatalf("hidden preview flow: err=%v calls=%d previews=%d commits=%d", err, calls, previews, commits)
	}
}

func TestCompletionAssessmentReachesFirstModelAndStillPreviewsBeforeOneTurnCommit(t *testing.T) {
	job := draftRunJob(t)
	a := assessmentFixture("")
	job.Graph.Project.OrchestrationVersion = 1
	job.Graph.Facts = a.UserInputs
	job.Graph.Hints = a.Hints
	feedback := a.UserInputs[2]
	job.Graph.Intents = []board.Intent{{ID: "feedback-step", From: []string{"origin"}, To: board.Ptr(feedback.ID), Description: "external_feedback", Creator: "human", Worker: board.Ptr("human")}}
	job.State = &board.State{Graph: job.Graph,
		Steps:       []board.Step{{ID: "feedback-step", From: []string{"origin"}, GoalID: "goal", Description: "external_feedback", Status: "completed", Result: board.Ptr(feedback.ID)}},
		FactRecords: append(append([]board.FactRecord{}, a.FactRecords...), board.FactRecord{ID: feedback.ID, Description: feedback.Description, Status: "valid", Legacy: true}),
	}
	var err error
	job.Decision, err = board.BuildDecisionContextFromCursor(*job.State, nil, nil, board.DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	job.Decision.Version = 2
	a.StateVersion = job.Decision.StateVersion
	if err = board.UseCompletionAssessment(*job.State, job.Decision, a); err != nil {
		t.Fatal(err)
	}
	runDir, calls := t.TempDir(), 0
	operations := []string{}
	bridge := &draftTestBridge{dir: runDir, handle: func(request GraphRequest) (any, error) {
		if request.Op == "decision_receipt" {
			return board.DecisionReceipt{}, nil
		}
		operations = append(operations, request.Op)
		if calls != 1 || request.Batch.ExpectedVersion != a.StateVersion || len(request.Batch.Actions) != 1 {
			t.Fatal("completion bypassed the observed version or changed its batch")
		}
		switch request.Op {
		case "decision_preview":
			return assessmentPreview(a, request.Batch.Actions[0].Payload), nil
		case "decision_commit":
			if !reflect.DeepEqual(operations, []string{"decision_preview", "decision_commit"}) {
				t.Fatal("commit skipped transactional preview")
			}
			return board.DecisionReceipt{Committed: true, Completed: true, StateVersion: a.StateVersion}, nil
		}
		return nil, errors.New("unexpected graph operation")
	}}
	p := scenarioProvider(func(_ context.Context, history []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls != 1 {
			return agent.Message{}, errors.New("redundant model confirmation")
		}
		raw, _ := json.Marshal(history)
		for _, required := range []string{a.UserInputs[0].Description, a.UserInputs[1].Description, feedback.Description, a.Hints[0].Content, "completion_assessment", "not_checked"} {
			if !strings.Contains(string(raw), required) {
				t.Fatalf("model did not receive authoritative input %q", required)
			}
		}
		text := ""
		for _, message := range history {
			text += message.Text()
		}
		for _, required := range []string{"Keep planning if requirements are unmet", "Otherwise stage complete with supporting IDs and proof", "empty omitted_fact_ids", "unchanged state/evidence", "no other draft actions, reset or recovery", "If reuse is unavailable or rejected, call preview and review completion_review in a subsequent model turn before commit"} {
			if !strings.Contains(text, required) {
				t.Fatalf("completion assessment omitted reuse condition or fallback: %q", required)
			}
		}
		encoded, _ := json.Marshal(a.FactRecords[0].Evidence[0].Excerpt)
		if strings.Count(text, string(encoded)) != 1 {
			t.Fatal("first completion request repeated or omitted exact evidence")
		}
		m := draftModelCall("finish", "graph_action", `{"op":"complete","idempotency_key":"finish","payload":{"from":["f001"],"description":"The actual retained response satisfies the production requirement"}}`)
		m.Content = append(m.Content, draftModelCall("commit", "graph_action", `{"op":"commit","idempotency_key":"commit"}`).Content...)
		return m, nil
	})
	result, err := runTestWorker(context.Background(), job, Options{RunDir: runDir, Provider: p, Output: bridge})
	if err != nil || result.Status != "success" || calls != 1 || len(operations) != 2 {
		t.Fatalf("one-turn completion failed: %+v %v calls=%d ops=%v", result, err, calls, operations)
	}
}

func TestCompletionAssessmentNeverAuthorizesUnseenChangedOrIncompleteEvidence(t *testing.T) {
	for _, scenario := range []string{"unseen", "legacy", "assessment_omitted", "version", "reset", "planning", "requirements", "feedback_missing", "feedback_changed", "hints", "evidence", "preview_omitted", "missing_fact", "conflict", "preview_failure"} {
		t.Run(scenario, func(t *testing.T) {
			a := assessmentFixture("v1")
			commits, previews := 0, 0
			d := &decisionDraft{orchestration: true, assessment: a}
			d.request = func(_ context.Context, request GraphRequest) (string, error) {
				if request.Op == "decision_commit" {
					commits++
					return `{"committed":true}`, nil
				}
				if request.Op != "decision_preview" {
					return "", errors.New("unexpected operation")
				}
				previews++
				if scenario == "conflict" {
					return "", errors.New("state_changed: another producer updated the board")
				}
				if scenario == "preview_failure" {
					return "", errors.New("transport failure")
				}
				preview := assessmentPreview(a, request.Batch.Actions[len(request.Batch.Actions)-1].Payload)
				switch scenario {
				case "requirements":
					preview.CompletionReview.UserInputs[1].Description = "A different goal"
				case "feedback_missing":
					preview.CompletionReview.UserInputs = preview.CompletionReview.UserInputs[:2]
				case "feedback_changed":
					preview.CompletionReview.UserInputs[2].Description = "Reopened requirement: also inspect /d"
				case "hints":
					preview.CompletionReview.Hints[0].Content = "A new constraint"
				case "evidence":
					preview.CompletionReview.FactRecords[0].Evidence[0].Excerpt = `{"response_received":false}`
				case "preview_omitted":
					preview.CompletionReview.OmittedFactIDs = []string{"f001"}
				case "missing_fact":
					preview.CompletionReview.FactRecords = nil
				}
				raw, _ := json.Marshal(preview)
				return string(raw), nil
			}
			if scenario != "unseen" {
				d.beforeRequest(assessmentObservedLoop(a))
				if !d.assessmentReady {
					t.Fatal("fixture did not deliver its initial assessment")
				}
			}
			switch scenario {
			case "legacy":
				d.orchestration = false
			case "assessment_omitted":
				a.OmittedFactIDs = []string{"other-observation"}
			case "version":
				a.StateVersion = "older"
			case "reset":
				_, _ = d.action(context.Background(), draftTestAction("reset", "reset", `{}`), "v1")
			case "planning":
				if _, err := d.action(context.Background(), draftTestAction("step", "retire", `{"action":"abandon","id":"old","reason":"Not required"}`), "v1"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := d.action(context.Background(), draftTestAction("complete", "finish", `{"from":["f001"],"description":"Proof"}`), "v1"); err != nil {
				t.Fatal(err)
			}
			if _, err := d.action(context.Background(), draftTestAction("commit", "commit", `{}`), "v1"); err == nil || commits != 0 || d.committed {
				t.Fatalf("unsafe completion committed: err=%v previews=%d commits=%d", err, previews, commits)
			}
			if scenario == "evidence" || scenario == "requirements" || scenario == "feedback_missing" || scenario == "feedback_changed" || scenario == "hints" {
				if previews != 1 || d.reviewReady || d.reviewData != "" {
					t.Fatal("mismatched evidence did not preserve the normal later-review boundary")
				}
				raw, err := d.action(context.Background(), draftTestAction("preview", "explicit", `{}`), "v1")
				if err != nil {
					t.Fatal(err)
				}
				loop := &agent.Loop{History: []agent.Message{agent.Text("user", raw)}}
				d.beforeRequest(loop)
				if !d.reviewReady || len(loop.ContextData) != 1 {
					t.Fatal("fallback did not deliver the authoritative preview for a later model request")
				}
				if _, err := d.action(context.Background(), draftTestAction("commit", "commit", `{}`), "v1"); err != nil || commits != 1 {
					t.Fatal("later model review could not use the ordinary completion path")
				}
			}
		})
	}
}

func TestCompletionAssessmentRecoveryDiscardsFastAuthority(t *testing.T) {
	d := &decisionDraft{orchestration: true, assessment: assessmentFixture("v1")}
	loop := assessmentObservedLoop(d.assessment)
	d.beforeRequest(loop)
	if !d.assessmentReady {
		t.Fatal("fixture did not deliver its initial assessment")
	}
	d.invalidate() // The real Worker does this before replaying a recovered session.
	d.observeRead("overview")
	d.observeRead("facts")
	if _, err := d.action(context.Background(), draftTestAction("complete", "finish", `{"from":["f001"],"description":"Proof"}`), "v1"); err != nil {
		t.Fatal(err)
	}
	d.beforeRequest(loop)
	if d.canAssessCompletion() || d.reviewReady {
		t.Fatal("recovery reauthorized a stale process-local assessment")
	}
	if _, err := d.action(context.Background(), draftTestAction("commit", "commit", `{}`), "v1"); err == nil {
		t.Fatal("recovered draft skipped a new preview and subsequent model review")
	}
}

func TestCompletionAssessmentReuseRequiresAllUncertaintyToBeObservedAndUnchanged(t *testing.T) {
	for _, scenario := range []string{"unchanged", "assessment_candidate_omitted", "assessment_dispute_omitted", "assessment_evidence_omitted", "assessment_evidence_count", "assessment_note_source_missing", "assessment_request_source_missing", "preview_candidate_omitted", "preview_dispute_omitted", "preview_evidence_omitted", "changed_note", "revised_note", "missing_note", "changed_dispute", "missing_dispute", "changed_request", "missing_request"} {
		t.Run(scenario, func(t *testing.T) {
			a := assessmentFixture("v1")
			if err := json.Unmarshal([]byte(`{"candidates":[{"id":"note","claim":"A different route remains uncertain","scope":"other route","status":"candidate","sources":["f001"],"reason":"Inspect whether this belongs to the requested scope","evidence_count":1,"evidence":[{"run_id":"run","path":"retained/note","excerpt":"ambiguous response"}]}],"disputes":[{"id":"question","status":"uncertain","question":"Is the other route in scope?","candidate_ids":["note"]}]}`), a); err != nil {
				t.Fatal(err)
			}
			a.CurationRequest = &board.CurationRequest{Sources: []string{"f001"}, Reason: "Recheck whether the response matches its interpretation", Revision: 1}
			switch scenario {
			case "assessment_candidate_omitted":
				a.OmittedCandidateIDs = []string{"other-note"}
			case "assessment_dispute_omitted":
				a.OmittedDisputeIDs = []string{"other-question"}
			case "assessment_evidence_omitted":
				a.Candidates[0].EvidenceOmitted = true
			case "assessment_evidence_count":
				a.Candidates[0].Evidence = nil
			case "assessment_note_source_missing":
				a.Candidates[0].Sources = []string{"unseen"}
			case "assessment_request_source_missing":
				a.CurationRequest.Sources = []string{"unseen"}
			}
			previews, commits := 0, 0
			d := &decisionDraft{orchestration: true, assessment: a}
			d.request = func(_ context.Context, request GraphRequest) (string, error) {
				if request.Op == "decision_commit" {
					commits++
					return `{"committed":true}`, nil
				}
				if request.Op != "decision_preview" {
					return "", errors.New("unexpected operation")
				}
				previews++
				receipt := assessmentPreview(a, request.Batch.Actions[0].Payload)
				review := receipt.CompletionReview
				switch scenario {
				case "preview_candidate_omitted":
					review.OmittedCandidateIDs = []string{"other-note"}
				case "preview_dispute_omitted":
					review.OmittedDisputeIDs = []string{"other-question"}
				case "preview_evidence_omitted":
					review.Candidates[0].EvidenceOmitted = true
				case "changed_note":
					review.Candidates[0].Reason = "This route is required and still untested"
				case "revised_note":
					review.Candidates[0].Supersedes = "earlier-note"
				case "missing_note":
					review.Candidates = nil
				case "changed_dispute":
					review.Disputes[0].Question = "Does the requested route still expose data?"
				case "missing_dispute":
					review.Disputes = nil
				case "changed_request":
					review.CurationRequest.Reason = "The response has now been contradicted"
				case "missing_request":
					review.CurationRequest = nil
				}
				raw, err := json.Marshal(receipt)
				return string(raw), err
			}
			d.beforeRequest(assessmentObservedLoop(a))
			if _, err := d.action(context.Background(), draftTestAction("complete", "finish", `{"from":["f001"],"description":"Proof"}`), "v1"); err != nil {
				t.Fatal(err)
			}
			_, err := d.action(context.Background(), draftTestAction("commit", "commit", `{}`), "v1")
			if scenario == "unchanged" {
				if err != nil || previews != 1 || commits != 1 || !d.committed {
					t.Fatalf("fully observed unchanged uncertainty forced a semantic machine judgment: err=%v previews=%d commits=%d", err, previews, commits)
				}
			} else if err == nil || commits != 0 || d.committed || d.reviewReady || d.reviewData != "" {
				t.Fatalf("unseen uncertainty authorized reused assessment: err=%v previews=%d commits=%d", err, previews, commits)
			}
		})
	}
}
