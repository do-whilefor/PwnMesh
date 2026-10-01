//go:build linux

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

const staleRepairPath = "/workspace/stale-repair-target.json"
const staleRepairOriginalPath = "/workspace/stale-repair-original.json"
const staleRepairOriginal = "{\"ok\":false}\n"
const staleRepairFixed = "{\"ok\":true}\n"

// Real planning and preparation exercise the production Go Loop. The second
// Step targets a retained historical version, after the preparation Worker has
// already fixed it. Its deterministic check must finish without any model call.
func TestLiveStaleRepairProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_STALE_REPAIR_TEST") != "1" {
		t.Skip("opt in to the real model and Docker stale-repair acceptance")
	}
	originalHash := fmt.Sprintf("%x", sha256.Sum256([]byte(staleRepairOriginal)))
	origin := fmt.Sprintf(`Exercise stale repair on synthetic local JSON files. Scope is ONLY /workspace. Do not read credentials, install packages, or contact other services.
Authorize exactly two ordinary Steps in sequence. First authorize one preparation Worker with no dependencies. It must write these exact UTF-8 bytes, including the final newline, to BOTH %s and %s: %q. It must then replace ONLY %s with these exact bytes: %q. Preserve the historical original file unchanged. Confirm both SHA-256 values using a deterministic command, then publish a fact with both original and corrected file evidence and finish successfully. This ordinary preparation task must have NO repair field.
After that Step has successfully completed, the main Agent must authorize exactly one new repair Step with depends_on containing the preparation Step ID and this structured repair field: {"path":%q,"sha256":%q,"rules":[{"kind":"json_equals","pointer":"/ok","value":true}]}. Its description should ask to fix /ok to true only if repair is still needed. The expected SHA-256 is the actual retained historical bad version, intentionally different from the already-fixed current version. Do not change this expected hash to the current hash and do not omit the Step just because preparation already fixed it: the purpose is to verify deterministic preflight acceptance of obsolete repair work.
The runtime must independently check the target and finish that repair Step as a no-op, retaining the current content, SHA-256, and check receipt without a model session. The main Agent should use the resulting persisted fact, allow observations to be curated, and complete the project. No candidate conflicts, unrelated Steps, retries, or report-writing Steps are needed. The persisted facts, dependency, original/current bytes, and repair receipt are the delivery.`, staleRepairPath, staleRepairOriginalPath, staleRepairOriginal, staleRepairPath, staleRepairFixed, staleRepairPath, originalHash)
	runObservedProject(t, "Stale repair skips already-correct local JSON", origin, "Retain an actual historical bad JSON version; independently accept the already-fixed target with SHA-256 and /ok=true checks; complete the obsolete repair without a model call.", "stale_repair_noop_with_retained_evidence", validateStaleRepairDelivery, 1)
}

// Decode the public JSON contract independently of the implementation types so
// the acceptance check detects dropped fields at Step/Job/result boundaries.
type staleRepairSpec struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Rules  []struct {
		Kind    string          `json:"kind"`
		Pointer string          `json:"pointer"`
		Value   json.RawMessage `json:"value"`
	} `json:"rules"`
}

type staleRepairReceipt struct {
	Path           string `json:"path"`
	ExpectedSHA256 string `json:"expected_sha256"`
	SHA256         string `json:"sha256"`
	Outcome        string `json:"outcome"`
	Satisfied      bool   `json:"satisfied"`
	FailedRules    []int  `json:"failed_rules"`
}

func staleRepairStepSpec(step board.Step) *staleRepairSpec {
	raw, _ := json.Marshal(step)
	var envelope struct {
		Repair *staleRepairSpec `json:"repair"`
	}
	_ = json.Unmarshal(raw, &envelope)
	return envelope.Repair
}

func staleRepairSpecValid(spec *staleRepairSpec) bool {
	return spec != nil && spec.Path == staleRepairPath && spec.SHA256 == fmt.Sprintf("%x", sha256.Sum256([]byte(staleRepairOriginal))) && len(spec.Rules) == 1 && spec.Rules[0].Kind == "json_equals" && spec.Rules[0].Pointer == "/ok" && bytes.Equal(bytes.TrimSpace(spec.Rules[0].Value), []byte("true"))
}

func staleRepairModelCalls(journal []byte) (int, bool) {
	calls := 0
	for _, line := range bytes.Split(journal, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event agent.Event
		if json.Unmarshal(line, &event) != nil || event.Type == "" {
			return 0, false
		}
		if event.Type == "model_call_start" {
			calls++
		}
	}
	return calls, true
}

// CompleteProject appends a concluded Intent pointing at the root goal. It is
// a main-Agent completion record, not another executed repair/preparation Step.
// Match its complete service-generated shape; never exempt any goal-labelled
// repair or an ordinary Step merely because its result was changed to "goal".
func staleRepairCompletionMarker(state board.State, step board.Step) bool {
	if state.Graph.Project.Status != "completed" || step.Result == nil || *step.Result != "goal" || step.Status != "completed" || !step.SupportValid || step.GoalID != "goal" || step.Worker == nil || *step.Worker == "" || staleRepairStepSpec(step) != nil || len(step.DependsOn) != 0 || step.DisputeID != "" || len(step.From) == 0 || state.ValidateFactSources(step.From, true) != nil {
		return false
	}
	for _, intent := range state.Graph.Intents {
		if intent.ID == step.ID && intent.To != nil && *intent.To == "goal" && intent.Worker != nil && *intent.Worker == *step.Worker && intent.Creator == *step.Worker && intent.CreatedAt != "" && intent.CreatedAt == step.CreatedAt && intent.ConcludedAt != nil && *intent.ConcludedAt == intent.CreatedAt && intent.Heartbeat != nil && *intent.Heartbeat == intent.CreatedAt && intent.Description == step.Description && slices.Equal(intent.From, step.From) {
			return true
		}
	}
	return false
}

func validateStaleRepairDelivery(state board.State, files map[string][]byte) []string {
	failures := []string{}
	check := func(ok bool, message string) {
		if !ok {
			failures = append(failures, message)
		}
	}
	check(state.Graph.Project.Status == "completed" && state.Graph.Project.OrchestrationVersion == 1, "stale-repair project did not complete in orchestration mode")
	var steps []board.Step
	markers := 0
	for _, step := range state.Steps {
		if staleRepairCompletionMarker(state, step) {
			markers++
		} else {
			steps = append(steps, step)
		}
	}
	check(markers <= 1, "multiple system completion markers")
	check(len(steps) == 2, "expected exactly one preparation and one obsolete repair Step")
	check(bytes.Equal(files[staleRepairOriginalPath], []byte(staleRepairOriginal)), "historical bad-version bytes were not retained")
	check(bytes.Equal(files[staleRepairPath], []byte(staleRepairFixed)), "target is not the exact already-fixed JSON; a changed hash alone is insufficient")
	if len(steps) != 2 {
		return failures
	}
	var prepare, repair *board.Step
	for i := range steps {
		step := &steps[i]
		if staleRepairStepSpec(*step) == nil {
			check(prepare == nil, "multiple preparation Steps or missing repair contract")
			prepare = step
		} else {
			check(repair == nil, "multiple repair Steps")
			repair = step
		}
	}
	check(prepare != nil && repair != nil, "Step repair contract was not persisted")
	if prepare == nil || repair == nil {
		return failures
	}
	check(staleRepairSpecValid(staleRepairStepSpec(*repair)), "repair Step does not bind the historical hash and required JSON predicate")
	check(len(prepare.DependsOn) == 0 && len(repair.DependsOn) == 1 && repair.DependsOn[0] == prepare.ID, "repair is not explicitly dependent on successful preparation")
	facts := map[string]board.FactRecord{}
	for _, fact := range state.FactRecords {
		facts[fact.ID] = fact
	}
	for _, step := range []*board.Step{prepare, repair} {
		if step.Worker == nil || step.Result == nil {
			check(false, "Step has no accepted worker result: "+step.ID)
			return failures
		}
		_, successful := orchestrationSuccessfulRun(state.Graph.Project, *step, *step.Worker, facts, files)
		check(successful, "Step has no matching successful immutable job/session/result Fact: "+step.ID)
		fact := facts[*step.Result]
		for _, ref := range fact.Evidence {
			check(orchestrationEvidenceValid(ref, *step.Worker, files), "Step result evidence is absent, changed or owned by another run: "+step.ID)
		}
	}
	prepareRun, repairRun := orchestrationRunID(*prepare.Worker), orchestrationRunID(*repair.Worker)
	check(prepareRun != "" && repairRun != "" && prepareRun != repairRun, "preparation and repair do not have distinct runs")
	historicalEvidence, correctedEvidence := false, false
	for _, ref := range facts[*prepare.Result].Evidence {
		historicalEvidence = historicalEvidence || bytes.Equal(files[ref.Path], []byte(staleRepairOriginal))
		correctedEvidence = correctedEvidence || bytes.Equal(files[ref.Path], []byte(staleRepairFixed))
	}
	check(historicalEvidence && correctedEvidence, "preparation Fact does not retain both historical and already-fixed bytes")
	base := "/workspace/.pwnmesh/runs/" + repairRun + "/"
	var job struct {
		Repair            *staleRepairSpec         `json:"repair"`
		DependencyResults []board.DependencyResult `json:"dependency_results"`
	}
	check(json.Unmarshal(files[base+"job.json"], &job) == nil && staleRepairSpecValid(job.Repair), "immutable repair job lost its target version or check rules")
	check(len(job.DependencyResults) == 1 && job.DependencyResults[0].StepID == prepare.ID && job.DependencyResults[0].FactID == *prepare.Result && job.DependencyResults[0].RunID == prepareRun, "repair job does not freeze the successful preparation result")
	var session struct {
		Result *struct {
			RepairCheck *staleRepairReceipt `json:"repair_check"`
		} `json:"result"`
	}
	if json.Unmarshal(files[base+"session.json"], &session) != nil || session.Result == nil || session.Result.RepairCheck == nil {
		check(false, "repair session did not persist its deterministic check result")
	} else {
		receipt := session.Result.RepairCheck
		expected := fmt.Sprintf("%x", sha256.Sum256([]byte(staleRepairOriginal)))
		actual := fmt.Sprintf("%x", sha256.Sum256(files[staleRepairPath]))
		check(receipt.Path == staleRepairPath && receipt.ExpectedSHA256 == expected && receipt.SHA256 == actual && actual != expected && receipt.Outcome == "noop" && receipt.Satisfied && len(receipt.FailedRules) == 0, "repair check did not independently accept the changed, already-correct target as noop")
		currentEvidence, checkEvidence := false, false
		for _, ref := range facts[*repair.Result].Evidence {
			currentEvidence = currentEvidence || bytes.Equal(files[ref.Path], []byte(staleRepairFixed))
			var retained staleRepairReceipt
			if json.Unmarshal(files[ref.Path], &retained) == nil && reflect.DeepEqual(retained, *receipt) {
				checkEvidence = true
			}
		}
		check(currentEvidence && checkEvidence, "no-op Fact does not retain current bytes and the matching deterministic receipt")
	}
	for _, run := range []string{prepareRun, repairRun} {
		journal, exists := files["/workspace/.pwnmesh/runs/"+run+"/events.jsonl"]
		calls, valid := staleRepairModelCalls(journal)
		check(exists && valid, "run journal is missing or malformed: "+run)
		if run == repairRun {
			check(calls == 0, "obsolete repair made a model call")
		} else {
			check(calls > 0, "preparation did not exercise a model session")
		}
	}
	return failures
}

func staleRepairDeliveryFixture(t *testing.T) (board.State, map[string][]byte) {
	t.Helper()
	state := board.State{Graph: board.Graph{Project: board.Project{ID: "p-repair", Status: "completed", OrchestrationVersion: 1}}}
	files := map[string][]byte{staleRepairPath: []byte(staleRepairFixed), staleRepairOriginalPath: []byte(staleRepairOriginal)}
	expected := fmt.Sprintf("%x", sha256.Sum256([]byte(staleRepairOriginal)))
	spec := map[string]any{"path": staleRepairPath, "sha256": expected, "rules": []map[string]any{{"kind": "json_equals", "pointer": "/ok", "value": true}}}
	receipt := staleRepairReceipt{Path: staleRepairPath, ExpectedSHA256: expected, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(staleRepairFixed))), Outcome: "noop", Satisfied: true}
	receiptJSON, _ := json.Marshal(receipt)
	for i, run := range []string{"prepare-run", "repair-run"} {
		stepID, factID, workerID := fmt.Sprintf("s%d", i+1), fmt.Sprintf("f%d", i+1), "live@"+run
		stepData := map[string]any{"id": stepID, "status": "completed", "support_valid": true, "worker": workerID, "result": factID}
		job := map[string]any{"run_id": run, "kind": "explore", "graph_rpc": true, "result_contract_version": 2, "graph": state.Graph, "intent": map[string]string{"id": stepID}}
		result := map[string]any{"status": "success", "text": fmt.Sprintf(`{"accepted":true,"outcome":"completed","data":{"fact_id":%q}}`, factID)}
		rawEvidence := [][]byte{[]byte(staleRepairOriginal), []byte(staleRepairFixed)}
		if i == 1 {
			stepData["repair"], stepData["depends_on"] = spec, []string{"s1"}
			job["repair"], job["dependency_results"] = spec, []board.DependencyResult{{StepID: "s1", FactID: "f1", RunID: "prepare-run"}}
			result["repair_check"] = receipt
			rawEvidence = [][]byte{[]byte(staleRepairFixed), receiptJSON}
		}
		var step board.Step
		raw, _ := json.Marshal(stepData)
		if err := json.Unmarshal(raw, &step); err != nil {
			t.Fatal(err)
		}
		state.Steps = append(state.Steps, step)
		fact := board.FactRecord{ID: factID, Description: "Retained repair observation", Scope: staleRepairPath, ObservedAt: "2026-09-28T00:00:00Z", Status: "valid", SourceStepID: stepID, RunID: workerID}
		for _, evidence := range rawEvidence {
			fact.Evidence = append(fact.Evidence, retainOrchestrationEvidence(run, evidence, files))
		}
		state.FactRecords = append(state.FactRecords, fact)
		base := "/workspace/.pwnmesh/runs/" + run + "/"
		files[base+"job.json"], _ = json.Marshal(job)
		files[base+"session.json"], _ = json.Marshal(map[string]any{"run_id": run, "identity": map[string]string{"project_id": state.Graph.Project.ID, "step_id": stepID, "run_id": run}, "result": result})
		files[base+"events.jsonl"] = []byte{}
		if i == 0 {
			files[base+"events.jsonl"] = []byte("{\"type\":\"model_call_start\"}\n")
		}
	}
	return state, files
}

func addStaleRepairCompletionMarker(state *board.State) {
	const at = "2026-09-28T00:01:00Z"
	const planner = "live@completion-run"
	state.Graph.Intents = append(state.Graph.Intents, board.Intent{ID: "completed-project", From: []string{"f1", "f2"}, To: board.Ptr("goal"), Description: "All required observations retained", Creator: planner, Worker: board.Ptr(planner), Heartbeat: board.Ptr(at), CreatedAt: at, ConcludedAt: board.Ptr(at)})
	state.Steps = append(state.Steps, board.Step{ID: "completed-project", From: []string{"f1", "f2"}, GoalID: "goal", Description: "All required observations retained", Status: "completed", Result: board.Ptr("goal"), Worker: board.Ptr(planner), CreatedAt: at, SupportValid: true})
}

func TestStaleRepairDeliveryAllowsOnlySystemCompletionMarker(t *testing.T) {
	for _, name := range []string{"no completion marker", "real completion marker", "extra ordinary step", "goal-labelled repair", "fake goal result", "mismatched creator", "missing completion time", "duplicate completion marker"} {
		t.Run(name, func(t *testing.T) {
			state, files := staleRepairDeliveryFixture(t)
			if name != "no completion marker" {
				addStaleRepairCompletionMarker(&state)
			}
			switch name {
			case "extra ordinary step":
				extra := state.Steps[0]
				extra.ID = "unexpected-work"
				state.Steps = append(state.Steps, extra)
			case "goal-labelled repair":
				state.Steps[2].Repair = state.Steps[1].Repair
			case "fake goal result":
				state.Graph.Intents = nil
			case "mismatched creator":
				state.Graph.Intents[0].Creator = "live@other-run"
			case "missing completion time":
				state.Graph.Intents[0].ConcludedAt = nil
			case "duplicate completion marker":
				state.Steps = append(state.Steps, state.Steps[2])
			}
			if failures := validateStaleRepairDelivery(state, files); (len(failures) == 0) != (name == "no completion marker" || name == "real completion marker") {
				t.Fatalf("%s: %v", name, failures)
			}
		})
	}
}

func TestStaleRepairDeliveryRejectsBrokenAcceptance(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*board.State, map[string][]byte)
	}{
		{"completion_only", func(s *board.State, _ map[string][]byte) { s.Steps = nil }},
		{"original_not_retained", func(_ *board.State, f map[string][]byte) { delete(f, staleRepairOriginalPath) }},
		{"changed_hash_but_rule_false", func(_ *board.State, f map[string][]byte) {
			f[staleRepairPath] = []byte("{\"ok\":false,\"changed\":true}\n")
		}},
		{"consistent_receipt_but_rule_false", func(s *board.State, f map[string][]byte) {
			// Keep all SHA references internally consistent. Acceptance must
			// still inspect /ok rather than trusting a changed hash or receipt.
			bad := []byte("{\"ok\":false,\"changed\":true}\n")
			f[staleRepairPath] = bad
			path := "/workspace/.pwnmesh/runs/repair-run/session.json"
			var session map[string]any
			_ = json.Unmarshal(f[path], &session)
			receipt := session["result"].(map[string]any)["repair_check"].(map[string]any)
			receipt["sha256"] = fmt.Sprintf("%x", sha256.Sum256(bad))
			f[path], _ = json.Marshal(session)
			raw, _ := json.Marshal(receipt)
			s.FactRecords[1].Evidence = []board.EvidenceRef{retainOrchestrationEvidence("repair-run", bad, f), retainOrchestrationEvidence("repair-run", raw, f)}
		}},
		{"unsupported_result", func(s *board.State, _ map[string][]byte) { s.Steps[1].SupportValid = false }},
		{"dependency_missing", func(s *board.State, _ map[string][]byte) { s.Steps[1].DependsOn = nil }},
		{"missing_immutable_job", func(_ *board.State, f map[string][]byte) { delete(f, "/workspace/.pwnmesh/runs/repair-run/job.json") }},
		{"missing_persisted_check", func(_ *board.State, f map[string][]byte) {
			path := "/workspace/.pwnmesh/runs/repair-run/session.json"
			var session map[string]any
			_ = json.Unmarshal(f[path], &session)
			delete(session["result"].(map[string]any), "repair_check")
			f[path], _ = json.Marshal(session)
		}},
		{"forged_hash", func(_ *board.State, f map[string][]byte) {
			path := "/workspace/.pwnmesh/runs/repair-run/session.json"
			var session map[string]any
			_ = json.Unmarshal(f[path], &session)
			session["result"].(map[string]any)["repair_check"].(map[string]any)["sha256"] = "unverified"
			f[path], _ = json.Marshal(session)
		}},
		{"check_receipt_not_retained", func(s *board.State, _ map[string][]byte) { s.FactRecords[1].Evidence = s.FactRecords[1].Evidence[:1] }},
		{"no_model_session_for_preparation", func(_ *board.State, f map[string][]byte) {
			f["/workspace/.pwnmesh/runs/prepare-run/events.jsonl"] = nil
		}},
		{"noop_called_model", func(_ *board.State, f map[string][]byte) {
			f["/workspace/.pwnmesh/runs/repair-run/events.jsonl"] = []byte("{\"type\":\"model_call_start\"}\n")
		}},
		{"missing_journal", func(_ *board.State, f map[string][]byte) {
			delete(f, "/workspace/.pwnmesh/runs/repair-run/events.jsonl")
		}},
		{"malformed_journal", func(_ *board.State, f map[string][]byte) {
			f["/workspace/.pwnmesh/runs/repair-run/events.jsonl"] = []byte("incomplete")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, files := staleRepairDeliveryFixture(t)
			test.change(&state, files)
			if failures := validateStaleRepairDelivery(state, files); len(failures) == 0 {
				t.Fatal("invalid no-op delivery passed")
			}
		})
	}
}
