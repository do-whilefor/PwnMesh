package worker

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func TestDependencySchemaIsLimitedToOrchestrationReason(t *testing.T) {
	for _, kind := range []string{"reason", "explore", "curate"} {
		for _, protocol := range []int{0, 1} {
			schema := graphActionPayloadSchema(kind)
			if protocol == 1 {
				schema = orchestrationPayloadSchema(kind)
			}
			properties := schema["properties"].(map[string]any)
			_, present := properties["depends_on"]
			if present != (protocol == 1 && kind == "reason") {
				t.Fatalf("unexpected dependency field for protocol %d, kind %s", protocol, kind)
			}
		}
	}
	schema, err := json.Marshal(orchestrationPayloadSchema("reason"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`null`, `{}`, `"step"`, `[7]`} {
		if err := agent.ValidateArguments(schema, json.RawMessage(`{"depends_on":`+value+`}`)); err == nil {
			t.Fatalf("malformed dependency collection passed schema: %s", value)
		}
	}
	if err := agent.ValidateArguments(schema, json.RawMessage(`{"depends_on":["step_existing","$first"]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestDecisionDependencyDraftCommitsEarlierStepAliases(t *testing.T) {
	j := Job{Kind: "reason", GraphRPC: true, Graph: board.Graph{Project: board.Project{OrchestrationVersion: 1}},
		Decision: &board.DecisionContext{Version: 2, StateVersion: strings.Repeat("a", 64)}}
	opts := Options{RunDir: t.TempDir()}
	requests := 0
	opts.Output = &draftTestBridge{dir: opts.RunDir, handle: func(request GraphRequest) (any, error) {
		requests++
		if request.Op != "decision_commit" || request.Batch == nil || len(request.Batch.Actions) != 2 {
			t.Fatalf("unexpected dependency publication: %+v", request)
		}
		var payload struct {
			From      []string `json:"from"`
			DependsOn []string `json:"depends_on"`
		}
		if err := json.Unmarshal(request.Batch.Actions[1].Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(payload.From, []string{"origin"}) || !reflect.DeepEqual(payload.DependsOn, []string{"step_existing", "$first"}) {
			t.Fatalf("dependency references changed or became evidence: %+v", payload)
		}
		return board.DecisionReceipt{Committed: true, StateVersion: j.Decision.StateVersion}, nil
	}}
	if err := ConfigureRuntimeTools(j, &opts); err != nil {
		t.Fatal(err)
	}
	action := opts.Tools[1]
	first := `{"op":"step","idempotency_key":"first","payload":{"action":"add","from":["origin"],"description":"Collect evidence"}}`
	dependent := `{"op":"step","idempotency_key":"second","payload":{"action":"add","from":["origin"],"depends_on":["step_existing","$first"],"description":"Review accepted outcomes"}}`
	for _, raw := range []string{first, dependent, dependent} {
		if err := agent.ValidateArguments(action.Schema, json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := action.Execute(context.Background(), json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 0 || len(opts.decision.actions) != 2 {
		t.Fatal("private dependencies published prematurely or duplicated on retry")
	}
	if _, err := action.Execute(context.Background(), json.RawMessage(`{"op":"commit","idempotency_key":"commit"}`)); err != nil || requests != 1 || !opts.decision.committed {
		t.Fatalf("dependency plan failed to commit: %v, requests=%d", err, requests)
	}
}

func TestDecisionDependencyDraftRejectsInvalidReferencesWithoutReservingKey(t *testing.T) {
	for name, payload := range map[string]string{
		"null":           `{"action":"add","from":["origin"],"description":"Review","depends_on":null}`,
		"non-string":     `{"action":"add","from":["origin"],"description":"Review","depends_on":[7]}`,
		"empty ID":       `{"action":"add","from":["origin"],"description":"Review","depends_on":[""]}`,
		"duplicate":      `{"action":"add","from":["origin"],"description":"Review","depends_on":["existing","existing"]}`,
		"goal alias":     `{"action":"add","from":["origin"],"description":"Review","depends_on":["$target"]}`,
		"forward alias":  `{"action":"add","from":["origin"],"description":"Review","depends_on":["$future"]}`,
		"self alias":     `{"action":"add","from":["origin"],"description":"Review","depends_on":["$retry"]}`,
		"root fact":      `{"action":"add","from":["origin"],"description":"Review","depends_on":["origin"]}`,
		"root goal":      `{"action":"add","from":["origin"],"description":"Review","depends_on":["goal"]}`,
		"not add":        `{"action":"abandon","id":"existing","reason":"Unused","depends_on":["$first"]}`,
		"alias evidence": `{"action":"add","from":["$first"],"description":"Review","depends_on":["$first"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			d := &decisionDraft{orchestration: true}
			for _, seed := range []board.StateAction{
				draftTestAction("goal", "target", `{"action":"add","condition":"Inspect"}`),
				draftTestAction("step", "first", `{"action":"add","from":["origin"],"description":"Inspect"}`),
			} {
				if _, err := d.action(context.Background(), seed, "original"); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := json.Marshal(d.actions)
			if _, err := d.action(context.Background(), draftTestAction("step", "retry", payload), "later"); err == nil || !strings.Contains(err.Error(), "draft unchanged") {
				t.Fatalf("invalid dependency accepted: %v", err)
			}
			after, _ := json.Marshal(d.actions)
			if string(before) != string(after) || len(d.keys) != 2 || d.version != "original" {
				t.Fatal("invalid dependency reserved a key or changed the draft")
			}
			if _, err := d.action(context.Background(), draftTestAction("step", "retry", `{"action":"add","from":["origin"],"description":"Review","depends_on":["$first"]}`), "later"); err != nil {
				t.Fatal(err)
			}
		})
	}
	legacy := &decisionDraft{}
	if _, err := legacy.action(context.Background(), draftTestAction("step", "first", `{"action":"add","from":["origin"],"description":"Inspect","depends_on":["existing"]}`), "original"); err == nil {
		t.Fatal("legacy draft accepted execution dependencies")
	}
}

func TestDependencyPromptKeepsInvalidSuccessRecoveryExplicit(t *testing.T) {
	job := scenarioJob(t, "", "reason")
	job.Graph.Project.OrchestrationVersion = 1
	prompt, err := Prompt(job, false, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"without valid support", "explicitly abandoned with a reason", "fresh verification", "historical success remains recorded"} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("planner was not given the supported recovery path: %q", text)
		}
	}
}

func TestDependencyResultsRemainVisibleAndBoundToExecution(t *testing.T) {
	job := outcomeJob(t, "explore")
	job.Graph.Project.OrchestrationVersion = 1
	job = bindingSnapshotJob(t, job)
	job.DependencyResults = []board.DependencyResult{{StepID: "step_upstream", FactID: "fact_accepted", RunID: "run_accepted"}}
	prompt, err := Prompt(job, false, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"<dependency_results>", `"step_id":"step_upstream"`, `"fact_id":"fact_accepted"`, `"run_id":"run_accepted"`, "fact_id evidence with read_snapshot", "never evidence"} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("frozen accepted result omitted from Worker input: %q", text)
		}
	}
	runDir := t.TempDir()
	identity, err := identityFor(job, runDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []board.DependencyResult{
		{StepID: "other-step", FactID: "fact_accepted", RunID: "run_accepted"},
		{StepID: "step_upstream", FactID: "other-fact", RunID: "run_accepted"},
		{StepID: "step_upstream", FactID: "fact_accepted", RunID: "other-run"},
	} {
		job.DependencyResults = []board.DependencyResult{changed}
		next, err := identityFor(job, runDir)
		if err != nil || next.JobDigest == identity.JobDigest {
			t.Fatalf("dependency identity was not bound to recovery: %+v %v", changed, err)
		}
	}
	job.Graph.Project.OrchestrationVersion = 0
	legacy, err := Prompt(job, false, t.TempDir())
	if err != nil || strings.Contains(legacy, "dependency_results") {
		t.Fatalf("dependency input leaked into legacy protocol: %v", err)
	}
}
