package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

func newCurationHTTPFixture(t *testing.T) *executionProtocolFixture {
	t.Helper()
	f := newExecutionProtocolFixture(t)
	var graph board.Graph
	f.request("POST", "/projects", map[string]any{"title": "Curated project", "origin": "Synthetic evidence", "goal": "Review an observation", "orchestration_version": 1}, false, http.StatusCreated, &graph)
	f.project = graph.Project.ID
	return f
}

func curatorTemplate(f *executionProtocolFixture) board.Execution {
	f.t.Helper()
	f.kind, f.intent = "curate", ""
	f.request("POST", f.base()+"/curate/claim", map[string]string{"worker": f.lease, "trigger": "new_observations"}, false, http.StatusOK, nil)
	job, err := json.Marshal(worker.Job{RunID: f.run, Kind: "curate", Workspace: "/workspace", GraphRPC: true, ResultContractVersion: 2, Graph: board.Graph{Project: f.state().Graph.Project}})
	if err != nil {
		f.t.Fatal(err)
	}
	return board.Execution{ProjectID: f.project, ID: f.run, Namespace: "protocol-test", Backend: "planner", Kind: "curate", Lease: f.lease, Job: job}
}

func prepareCurator(f *executionProtocolFixture) (board.Execution, worker.Job) {
	f.t.Helper()
	var execution board.Execution
	f.request("POST", f.base()+"/executions/prepare", curatorTemplate(f), true, http.StatusCreated, &execution)
	var job worker.Job
	if err := json.Unmarshal(execution.Job, &job); err != nil {
		f.t.Fatal(err)
	}
	return execution, job
}

func prepareOrchestratedExecute(f *executionProtocolFixture) {
	f.t.Helper()
	_, planner := prepareSnapshot(f.t, f, snapshotTemplate(f, "reason"))
	payload, _ := json.Marshal(map[string]any{"action": "add", "from": []string{"origin"}, "description": "Observe the synthetic fixture"})
	var plan board.DecisionReceipt
	f.request("POST", f.base()+"/state/decisions/commit", board.DecisionBatch{ExpectedVersion: planner.Decision.StateVersion, Actions: []board.DecisionAction{{Op: "step", Ref: "producer", Payload: payload}}}, true, http.StatusOK, &plan)
	f.kind, f.run, f.lease, f.intent = "explore", "producer-run", "planner@producer-run", plan.IDs["producer"]
	var intent board.Intent
	f.request("POST", f.base()+"/intents/"+f.intent+"/heartbeat", map[string]string{"worker": f.lease}, false, http.StatusOK, &intent)
	raw, err := json.Marshal(worker.Job{RunID: f.run, Kind: f.kind, Workspace: "/workspace", GraphRPC: true, ResultContractVersion: 2, Graph: board.Graph{Project: f.state().Graph.Project}, Intent: &intent})
	if err != nil {
		f.t.Fatal(err)
	}
	template := board.Execution{ProjectID: f.project, ID: f.run, Namespace: "protocol-test", Backend: "planner", Kind: f.kind, Intent: f.intent, Lease: f.lease, Job: raw}
	prepareSnapshot(f.t, f, template)
}

func TestProjectCreationStrictlySelectsOrchestration(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	for _, value := range []any{nil, true, "1", 0, -1, 2, 0.5, json.Number("1.0"), json.Number("1e0")} {
		f.request("POST", "/projects", map[string]any{"title": "Rejected mode", "origin": "Fixture", "goal": "Test", "orchestration_version": value}, false, http.StatusUnprocessableEntity, nil)
	}
	for _, version := range []int{1} {
		var graph board.Graph
		f.request("POST", "/projects", map[string]any{"title": "Explicit mode", "origin": "Fixture", "goal": "Test", "orchestration_version": version}, false, http.StatusCreated, &graph)
		if graph.Project.OrchestrationVersion != version {
			t.Fatal("creation silently selected another write protocol")
		}
		var saved board.Graph
		f.request("GET", "/projects/"+graph.Project.ID, nil, false, http.StatusOK, &saved)
		if saved.Project.OrchestrationVersion != version {
			t.Fatal("protocol was not persisted")
		}
	}
	if f.state().Graph.Project.OrchestrationVersion != 1 {
		t.Fatal("omitted version did not default to the current protocol")
	}
	f.request("POST", f.base()+"/curate/claim", map[string]string{"worker": f.lease, "trigger": "initial"}, false, http.StatusOK, nil)
}

func TestCuratorLeaseDoesNotConsumePlannerLeaseOrChangeContent(t *testing.T) {
	f := newCurationHTTPFixture(t)
	f.request("POST", f.base()+"/reason/claim", map[string]string{"worker": "primary", "trigger": "initial"}, false, http.StatusOK, nil)
	before := f.state()
	curatorTemplate(f)
	f.request("POST", f.base()+"/curate/claim", map[string]string{"worker": "other", "trigger": "initial"}, false, http.StatusConflict, nil)
	f.request("POST", f.base()+"/curate/heartbeat", map[string]string{"worker": f.lease}, true, http.StatusOK, nil)
	after := f.state()
	if after.Graph.Project.Reason == nil || after.Graph.Project.Reason.Worker != "primary" || after.Graph.Project.Curator == nil || after.Graph.Project.Curator.Worker != f.lease {
		t.Fatal("control roles did not retain independent leases")
	}
	if before.Revision != after.Revision || board.DecisionStateVersion(before) != board.DecisionStateVersion(after) {
		t.Fatal("lease activity changed semantic input")
	}
	f.request("POST", f.base()+"/curate/release", map[string]string{"worker": "other"}, true, http.StatusForbidden, nil)
	f.request("POST", f.base()+"/curate/release", map[string]string{"worker": f.lease}, true, http.StatusOK, nil)
	if after = f.state(); after.Graph.Project.Curator != nil || after.Graph.Project.Reason == nil {
		t.Fatal("curator release changed the planner lease")
	}
}

func TestCuratorPreparationIsImmutableAndHasNoStep(t *testing.T) {
	f := newCurationHTTPFixture(t)
	template := curatorTemplate(f)
	var saved board.Execution
	f.request("POST", f.base()+"/executions/prepare", template, true, http.StatusCreated, &saved)
	var job worker.Job
	if err := json.Unmarshal(saved.Job, &job); err != nil {
		t.Fatal(err)
	}
	if job.State != nil || job.InputSnapshot == nil || len(job.Graph.Facts)+len(job.Graph.Intents)+len(job.Graph.Hints) != 0 || job.Decision != nil || job.Intent != nil || len(job.InputView) == 0 || job.PreparationKey == "" || saved.RetryKey != "curate:"+job.InputSnapshot.StateVersion {
		t.Fatalf("invalid frozen curator input: %+v", job)
	}
	f.request("POST", f.base()+"/hints", map[string]string{"content": "A later hint", "creator": "fixture"}, false, http.StatusCreated, nil)
	var replay board.Execution
	f.request("POST", f.base()+"/executions/prepare", template, true, http.StatusOK, &replay)
	if !reflect.DeepEqual(saved, replay) {
		t.Fatal("preparation replay replaced the immutable input")
	}
	f.request("GET", f.base()+"/executions/check?namespace=protocol-test&kind=curate", nil, false, http.StatusOK, nil)
	f.request("GET", f.base()+"/executions/check?namespace=protocol-test&kind=curate&intent=made-up", nil, false, http.StatusUnprocessableEntity, nil)
	var fields map[string]any
	_ = json.Unmarshal(template.Job, &fields)
	fields["intent"] = board.Intent{ID: "made-up"}
	template.Job, _ = json.Marshal(fields)
	f.request("POST", f.base()+"/executions/prepare", template, true, http.StatusUnprocessableEntity, nil)
}

func TestCuratorCannotForgeSuccessfulResultWithoutReceipt(t *testing.T) {
	f := newCurationHTTPFixture(t)
	prepareCurator(f)
	f.request("GET", f.base()+"/state/curation/receipt", nil, true, http.StatusNotFound, nil)
	f.request("GET", f.base()+"/state/curation/receipt", nil, false, http.StatusForbidden, nil)
	for _, op := range []string{"fact", "candidate", "fact_relation"} {
		f.request("POST", f.base()+"/state/actions", map[string]any{"op": op, "idempotency_key": "forbidden-" + op, "payload": evidenceFixtureFact(f.run), "expected_version": board.DecisionStateVersion(f.state())}, true, http.StatusForbidden, nil)
	}
	f.request("POST", f.base()+"/intents", map[string]any{"from": []string{"origin"}, "description": "Unauthorized plan", "creator": f.lease}, true, http.StatusForbidden, nil)
	f.pending(`{"accepted":true,"data":{"curated":true}}`)
	f.apply(http.StatusConflict)
	if executions := f.executionRecords(); len(executions) != 1 || executions[0].Status != "result_pending" {
		t.Fatal("forged final text became a successful curation")
	}
}

func TestCurationHTTPRelationsAreAtomicAndPreserveProducerEvidence(t *testing.T) {
	f := newCurationHTTPFixture(t)
	prepareOrchestratedExecute(f)
	old := f.action("fact", "old-observation", evidenceFixtureFact(f.run))
	correction := f.action("fact", "corrected-observation", evidenceFixtureFact(f.run))
	candidate := f.action("candidate", "old-judgment", map[string]any{"claim": "Anonymous access is denied", "scope": "local fixture", "status": "verified", "sources": []string{old.ID}, "reason": "The initial observed response"})
	// The producer cannot hide a corrective write inside the curator payload.
	f.request("POST", f.base()+"/state/actions", map[string]any{"op": "curate", "idempotency_key": "producer-curation", "payload": map[string]any{"groups": []any{}, "relations": []any{map[string]string{"kind": "refutes", "source": correction.ID, "target": old.ID, "reason": "Unauthorized producer correction"}}}}, true, http.StatusForbidden, nil)
	f.run, f.lease = "curator-relations", "planner@curator-relations"
	_, job := prepareCurator(f)
	before := f.state()
	payload := board.CuratePayload{ThroughRevision: job.InputSnapshot.Revision,
		Relations: []board.CurateRelation{{Kind: "refutes", Source: correction.ID, Target: old.ID, Reason: "A fresh observation corrects the initial response"}},
		Groups:    []board.CurateGroup{{CandidateIDs: []string{candidate.ID}, Status: "verified", Reason: "Cannot verify a finding using its corrected support"}},
	}
	f.request("POST", f.base()+"/state/actions", map[string]any{"op": "curate", "idempotency_key": "rejected-relations", "payload": payload, "expected_version": job.InputSnapshot.StateVersion}, true, http.StatusConflict, nil)
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("failed HTTP curation partially persisted relations, groups or cursor")
	}
	f.request("GET", f.base()+"/state/curation/receipt", nil, true, http.StatusNotFound, nil)
	payload.Groups[0].Status = "candidate"
	payload.Groups[0].Reason = "The original support was corrected; preserve the candidate and its source"
	receipt := f.action("curate", "accepted-relations", payload)
	var result board.CurateResult
	if err := json.Unmarshal(receipt.Result, &result); err != nil {
		t.Fatal(err)
	}
	after := f.state()
	if !receipt.Committed || after.Revision != before.Revision+1 || after.Curation.ThroughRevision != before.Revision || len(result.FactRelations) != 1 || result.FactRelations[0].RunID != f.lease || len(after.FactRelations) != 1 || after.Findings[0].SupportValid {
		t.Fatalf("HTTP atomic curation omitted its accepted relation or support invalidation: %+v", after)
	}
	if !reflect.DeepEqual(before.Candidates, after.Candidates) || !reflect.DeepEqual(before.Graph.Facts, after.Graph.Facts) {
		t.Fatal("HTTP correction rewrote the producer's original data")
	}
	for _, fact := range after.FactRecords {
		if fact.ID == old.ID && (fact.Status != "refuted" || len(fact.Evidence) == 0) {
			t.Fatal("corrected observation lost its evidence or retained effective status")
		}
	}
	var replay board.StateActionResult
	f.request("POST", f.base()+"/state/actions", map[string]any{"op": "curate", "idempotency_key": "accepted-relations", "payload": payload, "expected_version": job.InputSnapshot.StateVersion}, true, http.StatusOK, &replay)
	if !reflect.DeepEqual(receipt, replay) || !reflect.DeepEqual(after, f.state()) {
		t.Fatal("HTTP curation replay duplicated effects or changed the original receipt")
	}
}

func TestCuratorReceiptSettlesExecutionAndSurvivesIdempotentApplication(t *testing.T) {
	f := newCurationHTTPFixture(t)
	prepareOrchestratedExecute(f)
	fact := f.action("fact", "observed", evidenceFixtureFact(f.run))
	candidate := f.action("candidate", "interpretation", map[string]any{"claim": "Anonymous access is denied", "scope": "local fixture", "status": "verified", "sources": []string{fact.ID}, "reason": "The response is unauthorized"})
	f.run, f.lease = "curator-run", "planner@curator-run"
	_, job := prepareCurator(f)
	f.request("POST", f.base()+"/executions/"+f.run+"/status", map[string]any{"status": "running"}, true, http.StatusOK, nil)
	receipt := f.action("curate", "curate-observed", map[string]any{"through_revision": job.InputSnapshot.Revision, "groups": []any{map[string]any{"candidate_ids": []string{candidate.ID}, "status": "verified", "reason": "The recorded observation supports this conclusion"}}})
	if !receipt.Committed {
		t.Fatal("accepted curation did not return a durable receipt")
	}
	f.request("POST", f.base()+"/executions/"+f.run+"/resume", map[string]any{}, true, http.StatusConflict, nil)
	f.apply(http.StatusOK)
	completed := f.state()
	f.apply(http.StatusOK)
	var replay board.StateActionResult
	f.request("GET", f.base()+"/state/curation/receipt", nil, true, http.StatusOK, &replay)
	ack := receipt
	ack.Result = nil
	compact := len(replay.Result) == 0 || string(replay.Result) == "null"
	replay.Result = nil
	if len(receipt.Result) == 0 || !compact || !reflect.DeepEqual(ack, replay) || f.state().Revision != completed.Revision || completed.Graph.Project.Curator != nil {
		t.Fatal("lost-response recovery changed the curation or retained its lease")
	}
	if err := f.store.Do(context.Background(), func(tx *board.Tx) error {
		persisted, err := tx.CurationReceipt(f.project, f.lease)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(persisted, receipt) {
			t.Fatal("compact HTTP acknowledgement changed the persisted receipt")
		}
		e, err := tx.Execution(f.project, f.run)
		if err == nil && (e.Status != "succeeded" || e.Resumes != 0 || !strings.Contains(string(e.Result), "curated")) {
			t.Fatalf("curation execution was not recovered: %+v", e)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOrchestrationPreparationRejectsLegacyExecutionProtocols(t *testing.T) {
	for _, mode := range []string{"mock_bridge", "legacy_result", "bootstrap"} {
		t.Run(mode, func(t *testing.T) {
			f := newCurationHTTPFixture(t)
			template := snapshotTemplate(f, "reason")
			var job map[string]any
			_ = json.Unmarshal(template.Job, &job)
			switch mode {
			case "mock_bridge":
				job["graph_rpc"] = false
			case "legacy_result":
				job["result_contract_version"] = 1
			case "bootstrap":
				job["kind"], template.Kind, f.kind = "bootstrap", "bootstrap", "bootstrap"
			}
			template.Job, _ = json.Marshal(job)
			f.request("POST", f.base()+"/executions/prepare", template, true, http.StatusUnprocessableEntity, nil)
			if len(f.executionRecords()) != 0 {
				t.Fatal("rejected legacy protocol persisted an execution")
			}
		})
	}
}

func TestOrchestrationLegacyMutationEndpointsCannotBypassRoleAuthority(t *testing.T) {
	f := newCurationHTTPFixture(t)
	prepareOrchestratedExecute(f)
	f.request("POST", f.base()+"/intents/"+f.intent+"/release", map[string]string{"worker": f.lease}, true, http.StatusOK, nil)
	before := f.state()
	for _, fenced := range []bool{false, true} {
		f.request("POST", f.base()+"/intents", map[string]any{"from": []string{"origin"}, "description": "Unplanned work", "creator": "unregistered"}, fenced, http.StatusForbidden, nil)
		f.request("POST", f.base()+"/complete", map[string]any{"from": []string{"origin"}, "description": "Unreviewed completion", "worker": "unregistered"}, fenced, http.StatusForbidden, nil)
	}
	f.request("POST", f.base()+"/intents/"+f.intent+"/conclude", map[string]string{"worker": "unregistered", "description": "Unsupported legacy conclusion"}, false, http.StatusConflict, nil)
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("legacy HTTP mutation bypassed the orchestrated write protocol")
	}
}
