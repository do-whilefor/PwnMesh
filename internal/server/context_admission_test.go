package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board"
)

func TestHintAdmissionPreservesRunnableContextAndRollsBack(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	for n := 0; n < 3; n++ {
		f.request("POST", f.base()+"/hints", map[string]string{"content": strings.Repeat("x", 8192), "creator": "user"}, false, http.StatusCreated, nil)
	}
	before := f.state()
	response := f.request("POST", f.base()+"/hints", map[string]string{"content": strings.Repeat("y", 8192), "creator": "user"}, false, http.StatusUnprocessableEntity, nil)
	if !strings.Contains(response, "input_context_limit") {
		t.Fatalf("missing actionable admission error: %s", response)
	}
	after := f.state()
	if len(after.Graph.Hints) != 3 || after.Revision != before.Revision || len(storedEvents(f)) != 3 {
		t.Fatal("rejected Hint changed the graph or event cursor")
	}
	if _, err := board.BuildDecisionContextFromCursor(after, nil, nil, board.DefaultContextViewBytes); err != nil {
		t.Fatalf("accepted requirements cannot start Decide: %v", err)
	}
	var hint board.Hint
	f.request("POST", f.base()+"/hints", map[string]string{"content": "Keep every earlier requirement", "creator": "user"}, false, http.StatusCreated, &hint)
	if hint.ID != "h004" || len(f.state().Graph.Hints) != 4 {
		t.Fatal("failed admission consumed IDs or discarded an earlier Hint")
	}
}

func TestHintAdmissionIncludesExistingExecuteContext(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	step := f.newIntent(strings.Repeat("s", 16000))
	f.request("POST", f.base()+"/hints", map[string]string{"content": strings.Repeat("h", 16000), "creator": "user"}, false, http.StatusUnprocessableEntity, nil)
	state := f.state()
	if len(state.Graph.Hints) != 0 {
		t.Fatal("Hint fits Decide alone but prevents the pending Step from starting")
	}
	if _, err := board.ContextView(state, step.ID, board.DefaultContextViewBytes); err != nil {
		t.Fatal(err)
	}
}

func TestInitialInputAndTitleAdmissionCountsEncodedBytes(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	// Each control character expands to six JSON bytes. Checking only raw
	// character count would admit a project whose initial prompt cannot fit.
	f.request("POST", "/projects", map[string]string{"title": "Escaped input", "origin": strings.Repeat("\x01", 6000), "goal": "Observe fixture"}, false, http.StatusUnprocessableEntity, nil)
	var projects []board.Summary
	f.request("GET", "/projects", nil, false, http.StatusOK, &projects)
	if len(projects) != 1 {
		t.Fatal("rejected project left partial data")
	}
	before, beforeEvents := f.state(), storedEvents(f)
	f.request("PUT", f.base()+"/title", map[string]string{"title": strings.Repeat("t", 32768)}, false, http.StatusUnprocessableEntity, nil)
	if after := f.state(); !reflect.DeepEqual(after, before) || !reflect.DeepEqual(storedEvents(f), beforeEvents) {
		t.Fatal("rejected title changed the graph or event history")
	}
}

func TestReopenAdmissionRollsBackAndPreservesFeedbackInContext(t *testing.T) {
	for _, tc := range []struct {
		name        string
		description string
	}{
		{name: "raw bytes", description: strings.Repeat("x", board.DefaultContextViewBytes+1)},
		{name: "encoded bytes", description: strings.Repeat("\x01", 6000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			step := f.newIntent()
			conclusion := f.completeStep(step, "Observed the required response")
			completion := f.completeProject(conclusion.Fact.ID)
			before, beforeEvents := f.state(), storedEvents(f)
			if before.Graph.Project.Status != "completed" || board.Value(completion.To) != "goal" {
				t.Fatal("fixture did not reach a completed project")
			}

			response := f.request("POST", f.base()+"/reopen", map[string]string{"description": tc.description, "creator": "user"}, false, http.StatusUnprocessableEntity, nil)
			if !strings.Contains(response, "input_context_limit") {
				t.Fatalf("missing actionable admission error: %s", response)
			}
			if after := f.state(); !reflect.DeepEqual(after, before) {
				t.Fatal("rejected reopen changed the completed project, completion intent, facts or revisions")
			}
			if afterEvents := storedEvents(f); !reflect.DeepEqual(afterEvents, beforeEvents) {
				t.Fatal("rejected reopen changed the event history")
			}

			feedback := "Recheck the new response.\nPreserve the original acceptance criteria."
			var reopened board.Reopened
			f.request("POST", f.base()+"/reopen", map[string]string{"description": feedback, "creator": "user"}, false, http.StatusOK, &reopened)
			if reopened.Project.Status != "active" || reopened.Fact.ID != "f002" || reopened.Intent.ID != "i003" {
				t.Fatalf("successful reopen lost its state transition or rejected admission consumed IDs: %+v", reopened)
			}
			after := f.state()
			if len(after.Graph.Facts) != len(before.Graph.Facts)+1 || after.Revision != before.Revision+1 || after.DecisionRevision != before.DecisionRevision+1 {
				t.Fatal("successful reopen did not publish exactly one feedback change")
			}
			for _, intent := range after.Graph.Intents {
				if intent.ID == completion.ID {
					t.Fatal("successful reopen retained the old completion intent")
				}
			}
			events := storedEvents(f)
			if len(events) != len(beforeEvents)+1 || events[len(events)-1].Op != "reopen" {
				t.Fatal("successful reopen did not publish exactly one event")
			}
			raw, err := board.ContextView(after, "", board.DefaultContextViewBytes)
			if err != nil {
				t.Fatalf("accepted feedback cannot start Decide: %v", err)
			}
			var view struct {
				UserInputs []board.Fact `json:"user_inputs"`
			}
			if err := json.Unmarshal(raw, &view); err != nil {
				t.Fatal(err)
			}
			wantInputs := map[string]string{reopened.Fact.ID: feedback}
			for _, fact := range before.Graph.Facts {
				if fact.ID == "origin" || fact.ID == "goal" {
					wantInputs[fact.ID] = fact.Description
				}
			}
			gotInputs := make(map[string]string, len(view.UserInputs))
			for _, fact := range view.UserInputs {
				gotInputs[fact.ID] = fact.Description
			}
			if len(view.UserInputs) != len(wantInputs) || !reflect.DeepEqual(gotInputs, wantInputs) {
				t.Fatalf("reopened context lost or altered original user input: got %+v, want %+v", gotInputs, wantInputs)
			}
		})
	}
}
