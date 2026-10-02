//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func curationJob(t *testing.T) Job {
	t.Helper()
	j := draftRunJob(t)
	j.Kind, j.Decision = "curate", nil
	j.Graph.Project.OrchestrationVersion = 1
	j.State = &board.State{Graph: j.Graph, Revision: 7}
	j.State.Candidates = []board.Candidate{{ID: "candidate_a", Claim: "Fixture permits access", Scope: "unauthenticated request", Status: "verified", Sources: []string{"fact_a"}}}
	return j
}

func TestCuratorRejectsMixedOrMismatchedInput(t *testing.T) {
	for name, mutate := range map[string]func(*Job){
		"legacy":    func(j *Job) { j.Graph.Project.OrchestrationVersion = 0 },
		"future":    func(j *Job) { j.Graph.Project.OrchestrationVersion = 2 },
		"no_bridge": func(j *Job) { j.GraphRPC = false },
		"no_state":  func(j *Job) { j.State = nil },
		"intent":    func(j *Job) { j.Intent = &board.Intent{ID: "step"} },
		"decision":  func(j *Job) { j.Decision = &board.DecisionContext{} },
		"snapshot":  func(j *Job) { j.InputSnapshot = &board.InputSnapshot{} },
		"view":      func(j *Job) { j.InputView = json.RawMessage(`{}`) },
		"graph": func(j *Job) {
			j.Graph.Facts = append([]board.Fact{}, j.Graph.Facts...)
			j.Graph.Facts[0].Description = "Changed input"
		},
	} {
		t.Run(name, func(t *testing.T) {
			j := curationJob(t)
			mutate(&j)
			if err := ConfigureRuntimeTools(j, &Options{}); err == nil {
				t.Fatal("accepted incompatible curator input")
			}
		})
	}
}

func TestCuratorCommitsFromOriginalLoopAndStopsWithoutFinalModelTurn(t *testing.T) {
	j, dir := curationJob(t), t.TempDir()
	calls, commits := 0, 0
	version := board.DecisionStateVersion(*j.State)
	bridge := &draftTestBridge{dir: dir, handle: func(request GraphRequest) (any, error) {
		if request.Op == "curate_receipt" {
			return board.StateActionResult{}, nil
		}
		if request.Op != "graph_action" || request.Action.Op != "curate" || request.Action.ExpectedVersion != version || request.Action.IdempotencyKey != j.RunID+":curate" {
			t.Fatalf("curation escaped immutable scope: %+v", request)
		}
		var payload struct {
			ThroughRevision int64 `json:"through_revision"`
			Relations       []struct {
				Kind, Source, Target, Reason string
			} `json:"relations"`
		}
		if json.Unmarshal(request.Action.Payload, &payload) != nil || payload.ThroughRevision != 7 || len(payload.Relations) != 1 || payload.Relations[0].Kind != "supersedes" || payload.Relations[0].Source != "fact_a" || payload.Relations[0].Target != "fact_old" || payload.Relations[0].Reason != "Corrected observation" {
			t.Fatalf("runtime did not bind the input boundary: %s", request.Action.Payload)
		}
		commits++
		return board.StateActionResult{Committed: true, StateVersion: version}, nil
	}}
	r, err := runTestWorker(context.Background(), j, Options{RunDir: dir, Output: bridge, Provider: scenarioProvider(func(_ context.Context, history []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls != 1 || len(defs) != 3 || defs[0].Name != "read_graph" || defs[1].Name != "graph_action" || defs[2].Name != "read_evidence" {
			t.Fatalf("curator gained tools or made an extra call: calls=%d tools=%+v", calls, defs)
		}
		if !strings.Contains(history[0].Text(), "candidate_a") || !strings.Contains(history[0].Text(), "fact_a") {
			t.Fatal("curator did not receive original candidates and sources")
		}
		return draftModelCall("curate", "graph_action", `{"op":"curate","payload":{"relations":[{"kind":"supersedes","source":"fact_a","target":"fact_old","reason":"Corrected observation"}],"groups":[{"candidate_ids":["candidate_a"],"status":"verified","reason":"Read the original source"}]}}`), nil
	})})
	if err != nil || r.Status != "success" || r.Text != committedCurationText || calls != 1 || commits != 1 || r.Metrics == nil || r.Metrics.ModelCalls != 1 || !r.Metrics.Committed || r.Metrics.Outcome != "curation_committed" {
		t.Fatalf("curation did not end at durable receipt: %+v err=%v calls=%d commits=%d", r, err, calls, commits)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil || !strings.Contains(string(raw), "model_call_end") || !strings.Contains(string(raw), "tool_end") {
		t.Fatalf("curation lost time observations: %s, %v", raw, err)
	}
}

func TestCuratorRecoversLostCommittedReceiptWithoutRepeatingModelOrMutation(t *testing.T) {
	j, dir, start := curationJob(t), t.TempDir(), time.Now()
	committed, calls, writes := false, 0, 0
	version := strings.Repeat("e", 64)
	bridge := &draftTestBridge{dir: dir, handle: func(request GraphRequest) (any, error) {
		if request.Op == "curate_receipt" {
			return board.StateActionResult{Committed: committed, StateVersion: version}, nil
		}
		if request.Op != "graph_action" || request.Action.Op != "curate" {
			t.Fatalf("unexpected operation %+v", request)
		}
		committed, writes = true, writes+1
		return nil, errors.New("receipt lost after durable commit")
	}}
	first, err := runTestWorker(context.Background(), j, Options{RunDir: dir, Output: bridge, Now: func() time.Time { return start }, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		calls++
		if calls == 1 {
			return draftModelCall("curate", "graph_action", `{"op":"curate","payload":{"groups":[]}}`), nil
		}
		return agent.Message{}, &agent.ModelError{Kind: agent.ErrorTransport, Err: errors.New("interrupted")}
	})})
	if err != nil || !first.Retryable || writes != 1 {
		t.Fatalf("lost receipt did not retain recovery: %+v %v", first, err)
	}
	before := outcomeSession(t, dir)
	resumedCalls := 0
	result, err := runTestWorker(context.Background(), j, Options{RunDir: dir, Output: bridge, Now: func() time.Time { return start.Add(time.Second) }, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		resumedCalls++
		return agent.Message{}, errors.New("receipt recovery must not call the model")
	})})
	if err != nil || result.Status != "success" || result.Text != committedCurationText || result.StateVersion != version || writes != 1 || resumedCalls != 0 {
		t.Fatalf("recovery repeated curation: %+v err=%v writes=%d calls=%d", result, err, writes, resumedCalls)
	}
	after := outcomeSession(t, dir)
	if after.Identity != before.Identity || !after.ExecutionDeadline.Equal(before.ExecutionDeadline) || after.RecoveryCount != before.RecoveryCount+1 {
		t.Fatal("curation recovery changed input, budget or allowance")
	}
}

func TestCuratorCannotFinishWithOnlyModelJSON(t *testing.T) {
	j, dir := curationJob(t), t.TempDir()
	bridge := &draftTestBridge{dir: dir, handle: func(request GraphRequest) (any, error) {
		if request.Op != "curate_receipt" {
			t.Fatalf("fabricated completion caused a mutation: %+v", request)
		}
		return board.StateActionResult{}, nil
	}}
	calls := 0
	r, err := runTestWorker(context.Background(), j, Options{RunDir: dir, Output: bridge, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		calls++
		return agent.Text("assistant", committedCurationText), nil
	})})
	if err != nil || r.Status != "failed" || calls != maxContinuations+1 || !strings.Contains(r.Error, "continuation_exhausted") {
		t.Fatalf("model final escaped receipt requirement: %+v err=%v calls=%d", r, err, calls)
	}
}

func TestCuratorStaleInputEndsWithoutRefreshingOrRepairing(t *testing.T) {
	j, dir := curationJob(t), t.TempDir()
	calls, writes := 0, 0
	bridge := &draftTestBridge{dir: dir, handle: func(request GraphRequest) (any, error) {
		if request.Op == "curate_receipt" {
			return board.StateActionResult{}, nil
		}
		writes++
		return nil, errors.New("state_changed: observations changed during curation")
	}}
	r, err := runTestWorker(context.Background(), j, Options{RunDir: dir, Output: bridge, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		calls++
		return draftModelCall("curate", "graph_action", `{"op":"curate","payload":{"groups":[]}}`), nil
	})})
	if err != nil || r.Status != "failed" || r.FailureKind != "state_changed" || r.Retryable || calls != 1 || writes != 1 {
		t.Fatalf("stale curation reused original input: %+v err=%v calls=%d writes=%d", r, err, calls, writes)
	}
	if s := outcomeSession(t, dir); s.RepairCount != 0 || s.Concluding {
		t.Fatal("stale curation entered output repair or conclusion")
	}
}

func TestOrchestrationRuntimePermissionsAndCandidateEvidence(t *testing.T) {
	for _, tc := range []struct {
		kind, op      string
		orchestration int
		allowed       bool
	}{
		{"reason", "goal", 1, true}, {"reason", "step", 1, true}, {"reason", "fact_relation", 1, false}, {"reason", "candidate", 1, false},
		{"curate", "curate", 1, true}, {"curate", "step", 1, false}, {"curate", "fact", 1, false}, {"curate", "fact_relation", 1, false},
		{"explore", "candidate", 1, true}, {"explore", "fact", 1, true}, {"explore", "finding", 1, false}, {"explore", "step", 1, false},
		{"reason", "curation_request", 1, true}, {"curate", "curation_request", 1, false},
		{"explore", "curation_request", 1, false},
	} {
		name := tc.kind + "/" + tc.op
		if tc.orchestration == 0 {
			name += "/legacy"
		}
		t.Run(name, func(t *testing.T) {
			j := curationJob(t)
			j.Kind, j.Graph.Project.OrchestrationVersion = tc.kind, tc.orchestration
			j.State.Graph.Project.OrchestrationVersion = tc.orchestration
			payload := json.RawMessage(`{}`)
			if tc.op == "curate" {
				payload = json.RawMessage(`{"groups":[]}`)
			} else if tc.op == "curation_request" {
				payload = json.RawMessage(`{"sources":["left","right"],"reason":"The retained observations conflict"}`)
			}
			r := GraphRequest{RequestID: strings.Repeat("a", 32), Op: "graph_action", Action: board.StateAction{Op: tc.op, IdempotencyKey: "key", Payload: payload, ExpectedVersion: board.DecisionStateVersion(*j.State)}}
			if err := ValidateGraphRequest(j, r); (err == nil) != tc.allowed {
				t.Fatalf("bridge permission differs: allowed=%v err=%v", tc.allowed, err)
			}
			options := Options{Tools: []agent.Tool{}}
			if err := ConfigureRuntimeTools(j, &options); err != nil {
				t.Fatal(err)
			}
			tool := snapshotRuntimeTool(t, options, "graph_action")
			fields := map[string]any{"op": tc.op, "payload": payload}
			if tc.kind != "curate" {
				fields["idempotency_key"] = "key"
			}
			input, _ := json.Marshal(fields)
			if err := agent.ValidateArguments(tool.Schema, input); (err == nil) != tc.allowed {
				t.Fatalf("schema permission differs: allowed=%v err=%v", tc.allowed, err)
			}
			if !tc.allowed {
				if _, err := tool.Execute(context.Background(), input); err == nil {
					t.Fatal("forbidden action reached execution")
				}
			} else if tc.op == "curation_request" && !strings.Contains(string(tool.Schema), "concrete evidence conflict or merge") {
				t.Fatal("curation request schema omits its decision criterion")
			}
		})
	}
	j, dir := curationJob(t), t.TempDir()
	j.Kind = "explore"
	if err := os.WriteFile(filepath.Join(j.Workspace, "source.txt"), []byte("Original evidence bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	action, err := prepareEvidence(context.Background(), j, dir, board.StateAction{Op: "candidate", IdempotencyKey: "candidate", Payload: json.RawMessage(`{"claim":"one claim","scope":"local","status":"verified","evidence":[{"path":"source.txt"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Evidence []board.EvidenceRef `json:"evidence"`
	}
	if json.Unmarshal(action.Payload, &payload) != nil || len(payload.Evidence) != 1 || !filepath.IsAbs(payload.Evidence[0].Path) || payload.Evidence[0].RunID != j.RunID || payload.Evidence[0].Excerpt != "Original evidence bytes\n" {
		t.Fatalf("candidate lost runtime-bound original evidence: %s", action.Payload)
	}
}

func TestCuratorSchemaAndPayloadBoundary(t *testing.T) {
	j := curationJob(t)
	o := Options{RunDir: t.TempDir()}
	if err := ConfigureRuntimeTools(j, &o); err != nil {
		t.Fatal(err)
	}
	action := o.Tools[1]
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(action.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	if _, exists := schema.Properties["idempotency_key"]; exists {
		t.Fatal("curator schema exposes a runtime-owned idempotency key")
	}
	if strings.Contains(string(schema.Properties["payload"]), "through_revision") {
		t.Fatal("curator schema exposes its immutable runtime-owned revision")
	}
	for _, raw := range []string{
		`{"op":"curate","idempotency_key":"model-key","payload":{"groups":[]}}`,
		`{"op":"curate","payload":{"through_revision":7,"groups":[]}}`,
		`{"op":"curate","payload":{"relations":[]}}`,
		`{"op":"curate","payload":{"groups":[],"relations":{}}}`,
		`{"op":"curate","payload":{"groups":[],"relations":[{"kind":"delete","source":"a","target":"b","reason":"r"}]}}`,
		`{"op":"curate","payload":{"groups":[],"relations":[{"kind":"supersedes","source":"a","target":"b"}]}}`,
		`{"op":"curate","payload":{"groups":[],"relations":[{"kind":"supersedes","source":"a","target":["b"],"reason":"r"}]}}`,
		`{"op":"curate","payload":{"groups":[],"relations":[{"kind":"supersedes","source":"a","target":"b","reason":"r","run_id":"forged"}]}}`,
		`{"op":"curate","payload":{"groups":[{"candidate_ids":"a","status":"verified","reason":"r"}]}}`,
		`{"op":"curate","payload":{"groups":[{"candidate_ids":["a"],"status":"verified","reason":"r","resolution":"closed"}]}}`,
		`{"op":"step","idempotency_key":"key","payload":{}}`,
	} {
		if err := agent.ValidateArguments(action.Schema, json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted invalid curator arguments: %s", raw)
		}
	}
	for _, kind := range []string{"supersedes", "refutes", "narrows"} {
		raw := json.RawMessage(`{"op":"curate","payload":{"groups":[],"relations":[{"kind":"` + kind + `","source":"a","target":"b","reason":"Original evidence establishes the relation"}]}}`)
		if err := agent.ValidateArguments(action.Schema, raw); err != nil {
			t.Fatalf("valid atomic relation rejected: %v", err)
		}
	}
	if strings.Contains(action.Description, "completed.data.fact_id") || !strings.Contains(action.Description, "atomically") || !strings.Contains(action.Description, "independent review") {
		t.Fatal("curation protocol does not describe its atomic role boundary")
	}
	if _, err := action.Execute(context.Background(), json.RawMessage(`{"op":"curate","payload":{"through_revision":8,"groups":[]}}`)); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("model replaced its input boundary: %v", err)
	}
	if _, err := action.Execute(context.Background(), json.RawMessage(`{"op":"step","idempotency_key":"key","payload":{}}`)); err == nil {
		t.Fatal("direct invocation bypassed role capability")
	}
}

func TestCuratorReceiptCompactionAndCandidateSourcePages(t *testing.T) {
	raw, _ := json.Marshal(board.StateActionResult{Op: "curate", Committed: true, StateVersion: strings.Repeat("a", 64), Result: json.RawMessage(`{"large":"` + strings.Repeat("x", MaxGraphRPCBytes) + `"}`)})
	compact, err := CompactGraphActionResult(raw)
	var receipt board.StateActionResult
	if err != nil || json.Unmarshal(compact, &receipt) != nil || !receipt.Committed {
		t.Fatalf("compaction lost the commit receipt: %s %v", compact, err)
	}
	j := curationJob(t)
	page := checkedGraphPage(t, *j.State, GraphRequest{Section: "candidates"})
	if len(page.Items) != 1 || !strings.Contains(string(page.Items[0]), "candidate_a") {
		t.Fatalf("missing candidate page: %+v", page)
	}
	page = checkedGraphPage(t, *j.State, GraphRequest{Section: "sources", IDs: []string{"candidate_a"}})
	if len(page.Items) != 1 || string(page.Items[0]) != `"fact_a"` {
		t.Fatalf("lost candidate source mapping: %+v", page)
	}
}
