//go:build linux

package integration

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/contract"
	"pwnmesh/internal/worker"
)

const orchestrationClaim = "The sum of sample-v1 values is 10"

// A synthetic disagreement, not an audit of a remote service. The two
// supplied reports intentionally disagree about the same immutable dataset.
// Only the independent review reads the actual data and recomputes the sum.
func TestLiveOrchestrationProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_ORCHESTRATION_TEST") != "1" {
		t.Skip("opt in to the real model and Docker orchestration acceptance")
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/report-a":
			_, _ = w.Write([]byte(`{"dataset":"sample-v1","reported_sum":10,"report":"a"}`))
		case "/report-b":
			_, _ = w.Write([]byte(`{"dataset":"sample-v1","reported_sum":11,"report":"b"}`))
		case "/dataset":
			_, _ = w.Write([]byte(`{"dataset":"sample-v1","values":[2,3,5]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()
	origin := fmt.Sprintf(`Exercise the orchestration protocol on three synthetic immutable local JSON resources at %s. Scope is ONLY those URLs and /workspace. Do not read credentials, install packages, or contact other services.
The supplied reports intentionally disagree; preserve both, then independently verify the real data. Use exactly this claim: "The sum of sample-v1 values is 10" and scope: "sample-v1" for all candidate judgments.
First the main Agent should authorize two separate independent ordinary Steps in one plan, both with no depends_on: one Worker retrieves ONLY /report-a, one retrieves ONLY /report-b. Each writes the original JSON HTTP response body to its own evidence file, publishes a fact accurately describing that report (the report is not ground truth), and submits a candidate for the common claim: verified for report-a's assertion, refuted for report-b's assertion. These are deliberately conflicting candidate interpretations, not final verified Findings. Initial workers must not inspect /dataset or resolve the disagreement. Each task finishes after publishing its report and candidate.
The blackboard Agent preserves both sources, marks the conflict, and asks for independent review. Once a dispute exists and the first two tasks have succeeded, the main Agent creates exactly one new Step with its dispute_id AND depends_on containing exactly the two initial Step IDs. This is a structured successful-completion dependency, not just prose or from fact IDs. Ask an independent Worker to examine both original reports and GET /dataset, save its original JSON response body, and calculate the sum with a deterministic command that reads that saved body. The command must save AND print a JSON calculation receipt with these fields: dataset (from the input), values (the actual input array), sum (computed, not hardcoded), dataset_sha256 (SHA-256 of the exact saved body bytes). Retain this receipt as a separate evidence file. File names are unrestricted; cite both the original dataset body and calculation receipt as evidence in the review fact and candidate. The reviewer must publish a fact containing the dataset and independently calculated sum, and a candidate for the common claim with new review evidence. It must not merely repeat either report.
The blackboard Agent then resolves only with the independent review fact IDs, preserving original candidates. If evidence is insufficient it must keep the dispute uncertain. The main Agent may complete only after the dispute is resolved and all observations are curated. No additional report-writing Step is needed: the persisted Findings, dispute history, and absolute /workspace evidence references are the delivery.`, fixture.URL)
	goal := "Retain both conflicting report candidates, open a dispute, authorize one independent review in a new run, verify that 2+3+5=10 from the dataset, resolve with that evidence, and complete only after curation."
	runObservedProject(t, "Synthetic independent-review orchestration", origin, goal, "independent_review_with_retained_evidence", validateOrchestrationDelivery, 1)
}

func validateOrchestrationDelivery(state board.State, files map[string][]byte) []string {
	failures := []string{}
	check := func(ok bool, message string) {
		if !ok {
			failures = append(failures, message)
		}
	}
	if state.Graph.Project.Status != "completed" || state.Graph.Project.OrchestrationVersion != 1 {
		failures = append(failures, "new-mode project did not complete")
	}
	check(len(state.Candidates) >= 3, "original opposing candidates and independent review were not retained")
	check(len(state.Disputes) == 1, "expected exactly one dispute")
	steps := map[string]board.Step{}
	var reviews []board.Step
	for _, step := range state.Steps {
		steps[step.ID] = step
		if step.DisputeID != "" {
			reviews = append(reviews, step)
		}
	}
	check(len(reviews) == 1, "expected exactly one independent review step")
	facts := map[string]board.FactRecord{}
	for _, fact := range state.FactRecords {
		facts[fact.ID] = fact
		for _, evidence := range fact.Evidence {
			check(orchestrationEvidenceValid(evidence, fact.RunID, files), "fact evidence is absent, changed, or not owned by its source run: "+fact.ID)
		}
	}
	// Select the opposing producers from their source facts' retained bytes,
	// never from their prose or a candidate status detached from claim/scope.
	producers := map[string]board.Candidate{}
	reviewSupport := map[string][]string{}
	for _, candidate := range state.Candidates {
		check(candidate.Generation == state.Graph.Project.Generation, "candidate belongs to another project generation: "+candidate.ID)
		if step := steps[candidate.SourceStepID]; step.DisputeID != "" {
			sources, valid := orchestrationReviewCandidateSources(state.Graph.Project, candidate, step, steps, facts, files)
			check(valid, "candidate evidence is not linked to its effective source facts: "+candidate.ID)
			if valid {
				reviewSupport[candidate.ID] = sources
			}
		} else {
			check(orchestrationCandidateSources(candidate, facts), "candidate evidence is not linked to its effective source facts: "+candidate.ID)
		}
		for _, evidence := range candidate.Evidence {
			check(orchestrationEvidenceValid(evidence, candidate.RunID, files), "candidate evidence is absent, changed, or not owned by its source run: "+candidate.ID)
		}
		if candidate.Claim != orchestrationClaim || candidate.Scope != "sample-v1" || steps[candidate.SourceStepID].DisputeID != "" {
			continue
		}
		for _, source := range candidate.Sources {
			fact, exists := facts[source]
			if !exists || fact.Status != "valid" || fact.SourceStepID != candidate.SourceStepID || fact.RunID != candidate.RunID {
				continue
			}
			for _, evidence := range fact.Evidence {
				var report struct {
					Dataset string `json:"dataset"`
					Report  string `json:"report"`
					Sum     int    `json:"reported_sum"`
				}
				if json.Unmarshal(orchestrationJSONBody(files[evidence.Path]), &report) != nil || report.Dataset != "sample-v1" {
					continue
				}
				if (report.Report == "a" && report.Sum == 10 && candidate.Status == "verified") || (report.Report == "b" && report.Sum == 11 && candidate.Status == "refuted") {
					producers[report.Report] = candidate
				}
			}
		}
	}
	a, b := producers["a"], producers["b"]
	check(a.ID != "" && b.ID != "", "opposing candidates lack the common claim/scope and original report-a=10/report-b=11 evidence")
	check(a.SourceStepID != "" && b.SourceStepID != "" && a.SourceStepID != b.SourceStepID && orchestrationRunID(a.RunID) != orchestrationRunID(b.RunID), "initial candidates were not produced by distinct Steps and runs")
	if len(reviews) != 1 || len(state.Disputes) != 1 || a.ID == "" || b.ID == "" {
		return failures
	}
	review, dispute := reviews[0], state.Disputes[0]
	for _, candidate := range []board.Candidate{a, b} {
		step := steps[candidate.SourceStepID]
		check(len(step.DependsOn) == 0, "initial report Steps were not independent")
		_, ok := orchestrationSuccessfulRun(state.Graph.Project, step, candidate.RunID, facts, files)
		check(ok, "initial report Step has no matching successful run: "+step.ID)
		check(slices.Contains(dispute.CandidateIDs, candidate.ID) && slices.Contains(dispute.ProducerRunIDs, candidate.RunID), "dispute lost an original candidate or producer run")
	}
	check(len(review.DependsOn) == 2 && slices.Contains(review.DependsOn, a.SourceStepID) && slices.Contains(review.DependsOn, b.SourceStepID), "review depends_on does not bind both initial Steps")
	check(review.DisputeID == dispute.ID && dispute.Status == "resolved" && len(dispute.ReviewStepIDs) == 1 && dispute.ReviewStepIDs[0] == review.ID, "dispute is not resolved by the unique review Step")
	reviewRun := ""
	if review.Worker != nil {
		reviewRun = *review.Worker
	}
	check(reviewRun != "" && orchestrationRunID(reviewRun) != orchestrationRunID(a.RunID) && orchestrationRunID(reviewRun) != orchestrationRunID(b.RunID) && !slices.Contains(dispute.ProducerRunIDs, reviewRun), "review run is not independent of both producers")
	job, successful := orchestrationSuccessfulRun(state.Graph.Project, review, reviewRun, facts, files)
	check(successful, "review Step has no matching successful run")
	bound := len(job.DependencyResults) == 2
	for _, candidate := range []board.Candidate{a, b} {
		step, found := steps[candidate.SourceStepID], false
		for _, dependency := range job.DependencyResults {
			if dependency.StepID == step.ID && step.Result != nil && dependency.FactID == *step.Result && dependency.RunID == orchestrationRunID(candidate.RunID) {
				found = true
			}
		}
		bound = bound && found
	}
	check(bound, "review immutable job does not bind both successful dependency results")
	check(len(dispute.ReviewFactIDs) > 0, "dispute has no review fact sources")
	for _, id := range dispute.ReviewFactIDs {
		fact, exists := facts[id]
		check(exists && fact.Status == "valid" && !fact.SupportInvalid && !fact.Legacy && fact.SourceStepID == review.ID && fact.RunID == reviewRun, "dispute cites a fact outside the independent successful review: "+id)
	}
	reviewCandidate := false
	for _, candidate := range state.Candidates {
		if candidate.Claim == orchestrationClaim && candidate.Scope == "sample-v1" && candidate.Status == "verified" && candidate.SourceStepID == review.ID && candidate.RunID == reviewRun && slices.Contains(dispute.CandidateIDs, candidate.ID) && len(candidate.Sources) > 0 {
			linked := len(reviewSupport[candidate.ID]) > 0
			for _, source := range reviewSupport[candidate.ID] {
				linked = linked && slices.Contains(dispute.ReviewFactIDs, source)
			}
			reviewCandidate = reviewCandidate || linked
		}
	}
	check(reviewCandidate, "independent review candidate is not linked to the dispute's review facts")
	check(orchestrationRecomputation(dispute.ReviewFactIDs, facts, reviewRun, files), "independent review lacks original dataset bytes, matching calculation receipt, or successful command output")
	final := false
	for _, finding := range state.Findings {
		if finding.ID != dispute.FindingID || finding.DisputeID != dispute.ID || finding.Claim != orchestrationClaim || finding.Scope != "sample-v1" || finding.Status != "verified" || !finding.SupportValid {
			continue
		}
		linked := len(finding.Sources) > 0
		for _, source := range finding.Sources {
			linked = linked && slices.Contains(dispute.ReviewFactIDs, source)
		}
		final = final || (linked && orchestrationRecomputation(finding.Sources, facts, reviewRun, files))
	}
	check(final, "supported verified Finding is not derived from the dispute's independent recomputation facts")
	return failures
}

func orchestrationRunID(workerID string) string {
	_, run, found := strings.Cut(workerID, "@")
	if !found {
		run = workerID
	}
	if run == "" || path.Base(run) != run || strings.ContainsAny(run, ".\\") {
		return ""
	}
	return run
}

func orchestrationEvidenceValid(e board.EvidenceRef, workerID string, files map[string][]byte) bool {
	raw, run := files[e.Path], orchestrationRunID(workerID)
	return run != "" && e.RunID == run && len(raw) > 0 && e.Excerpt != "" && bytes.Contains(raw, []byte(e.Excerpt)) && e.Path == fmt.Sprintf("/workspace/.pwnmesh/runs/%s/evidence/%x.raw", run, sha256.Sum256(raw))
}

// Evidence may retain the body alone or a complete raw HTTP response. Parsing
// HTTP enforces framing/status rather than accepting JSON buried in prose.
func orchestrationJSONBody(raw []byte) []byte {
	if json.Valid(raw) {
		return raw
	}
	reader := bufio.NewReader(bytes.NewReader(raw))
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || !json.Valid(body) || reader.Buffered() != 0 {
		return nil
	}
	return body
}

func orchestrationCandidateSources(candidate board.Candidate, facts map[string]board.FactRecord) bool {
	if len(candidate.Sources) == 0 {
		return false
	}
	var evidence []board.EvidenceRef
	for _, id := range candidate.Sources {
		fact, exists := facts[id]
		if !exists || fact.Status != "valid" || fact.SupportInvalid || fact.Legacy || fact.RunID != candidate.RunID || fact.SourceStepID != candidate.SourceStepID {
			return false
		}
		evidence = append(evidence, fact.Evidence...)
	}
	if len(evidence) == 0 {
		return false
	}
	for _, ref := range candidate.Evidence {
		if !slices.ContainsFunc(evidence, func(source board.EvidenceRef) bool { return source.RunID == ref.RunID && source.Path == ref.Path }) {
			return false
		}
	}
	return true
}

// The task requires the review judgment's new data and calculation evidence,
// while explicitly asking the reviewer to examine both original reports. Those
// reports may be cited as context; they never substitute for independent support.
// Limit that context to the exact successful dependency results frozen in Job.
func orchestrationReviewCandidateSources(project board.Project, candidate board.Candidate, review board.Step, steps map[string]board.Step, facts map[string]board.FactRecord, files map[string][]byte) ([]string, bool) {
	job, success := orchestrationSuccessfulRun(project, review, candidate.RunID, facts, files)
	if !success || len(review.DependsOn) != 2 || len(job.DependencyResults) != 2 {
		return nil, false
	}
	allowedContext := map[string]bool{}
	for _, dependency := range job.DependencyResults {
		step, exists := steps[dependency.StepID]
		if !exists || !slices.Contains(review.DependsOn, step.ID) || step.Result == nil || *step.Result != dependency.FactID || step.Worker == nil || orchestrationRunID(*step.Worker) != dependency.RunID || step.ID == review.ID || *step.Worker == candidate.RunID {
			return nil, false
		}
		if _, ok := orchestrationSuccessfulRun(project, step, *step.Worker, facts, files); !ok {
			return nil, false
		}
		allowedContext[dependency.FactID] = true
	}
	if len(allowedContext) != 2 {
		return nil, false
	}
	own := candidate
	own.Sources = nil
	for _, id := range candidate.Sources {
		fact, exists := facts[id]
		if !exists || fact.Status != "valid" || fact.SupportInvalid || fact.Legacy {
			return nil, false
		}
		if fact.RunID == candidate.RunID && fact.SourceStepID == candidate.SourceStepID {
			own.Sources = append(own.Sources, id)
		} else if !allowedContext[id] {
			return nil, false
		}
	}
	if !orchestrationCandidateSources(own, facts) || !slices.ContainsFunc(own.Sources, func(id string) bool {
		return orchestrationRecomputation([]string{id}, facts, candidate.RunID, files)
	}) {
		return nil, false
	}
	// The candidate itself must cite both new artifacts, not merely point at
	// a fact that happens to contain them while supplying only old report bytes.
	selected := board.FactRecord{ID: candidate.ID, RunID: candidate.RunID, Status: "valid", Evidence: candidate.Evidence}
	if !orchestrationRecomputation([]string{selected.ID}, map[string]board.FactRecord{selected.ID: selected}, candidate.RunID, files) {
		return nil, false
	}
	return own.Sources, true
}

func orchestrationSuccessfulRun(project board.Project, step board.Step, workerID string, facts map[string]board.FactRecord, files map[string][]byte) (worker.Job, bool) {
	var job worker.Job
	var session struct {
		RunID    string `json:"run_id"`
		Identity struct {
			ProjectID string `json:"project_id"`
			StepID    string `json:"step_id"`
			RunID     string `json:"run_id"`
		} `json:"identity"`
		Result *worker.Result `json:"result"`
	}
	run := orchestrationRunID(workerID)
	base := "/workspace/.pwnmesh/runs/" + run + "/"
	if run == "" || step.Status != "completed" || !step.SupportValid || step.Worker == nil || *step.Worker != workerID || step.Result == nil || json.Unmarshal(files[base+"job.json"], &job) != nil || json.Unmarshal(files[base+"session.json"], &session) != nil {
		return job, false
	}
	if job.RunID != run || job.Graph.Project.ID != project.ID || job.Graph.Project.OrchestrationVersion != 1 || job.Graph.Project.Generation != project.Generation || job.Kind != "explore" || job.ResultContractVersion != 2 || !job.GraphRPC || job.Intent == nil || job.Intent.ID != step.ID || session.RunID != run || session.Identity.RunID != run || session.Identity.ProjectID != project.ID || session.Identity.StepID != step.ID || session.Result == nil || session.Result.Status != "success" || session.Result.Retryable {
		return job, false
	}
	resultFact, exists := facts[*step.Result]
	if !exists || resultFact.Status != "valid" || resultFact.SupportInvalid || resultFact.Legacy || resultFact.SourceStepID != step.ID || resultFact.RunID != workerID || len(resultFact.Evidence) == 0 {
		return job, false
	}
	parsed, err := contract.ParseWithPolicy(session.Result.Text, job.Kind, session.Result.Conclude, 0, job.Budget.MaxIntents, contract.Policy{Version: job.ResultContractVersion, GraphRPC: job.GraphRPC})
	if err != nil || parsed.Kind != "fact" || parsed.Outcome != "completed" {
		return job, false
	}
	if parsed.FactID != "" {
		return job, parsed.FactID == *step.Result
	}
	var inline board.FactRecord
	if json.Unmarshal(parsed.FactPayload, &inline) != nil {
		return job, false
	}
	observed, inputErr := time.Parse(time.RFC3339Nano, inline.ObservedAt)
	stored, storedErr := time.Parse(time.RFC3339Nano, resultFact.ObservedAt)
	return job, inputErr == nil && storedErr == nil && observed.Equal(stored) && strings.TrimSpace(inline.Description) == resultFact.Description && strings.TrimSpace(inline.Scope) == resultFact.Scope && slices.Equal(inline.Evidence, resultFact.Evidence)
}

func orchestrationRecomputation(ids []string, facts map[string]board.FactRecord, workerID string, files map[string][]byte) bool {
	var bodies [][]byte
	var receipts [][]byte
	for _, id := range ids {
		fact := facts[id]
		if fact.RunID != workerID || fact.Status != "valid" || fact.SupportInvalid || fact.Legacy {
			return false
		}
		for _, evidence := range fact.Evidence {
			if !orchestrationEvidenceValid(evidence, workerID, files) {
				return false
			}
			raw := orchestrationJSONBody(files[evidence.Path])
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) != nil {
				continue
			}
			if len(fields) == 2 && fields["dataset"] != nil && fields["values"] != nil {
				bodies = append(bodies, raw)
			}
			if len(fields) == 4 && fields["dataset"] != nil && fields["values"] != nil && fields["sum"] != nil && fields["dataset_sha256"] != nil {
				receipts = append(receipts, raw)
			}
		}
	}
	for _, body := range bodies {
		var data struct {
			Dataset string `json:"dataset"`
			Values  []int  `json:"values"`
		}
		if json.Unmarshal(body, &data) != nil || data.Dataset != "sample-v1" || !slices.Equal(data.Values, []int{2, 3, 5}) {
			continue
		}
		sum := 0
		for _, value := range data.Values {
			sum += value
		}
		for _, receipt := range receipts {
			var calculated struct {
				Dataset string `json:"dataset"`
				Values  []int  `json:"values"`
				Sum     int    `json:"sum"`
				SHA256  string `json:"dataset_sha256"`
			}
			if json.Unmarshal(receipt, &calculated) == nil && calculated.Dataset == data.Dataset && slices.Equal(calculated.Values, data.Values) && calculated.Sum == sum && calculated.SHA256 == fmt.Sprintf("%x", sha256.Sum256(body)) && orchestrationCommandOutput(workerID, receipt, files) {
				return true
			}
		}
	}
	return false
}

func orchestrationCommandOutput(workerID string, receipt []byte, files map[string][]byte) bool {
	// Use the durable journal, whose earlier messages survive context compaction.
	journal := files["/workspace/.pwnmesh/runs/"+orchestrationRunID(workerID)+"/events.jsonl"]
	bash := map[string]bool{}
	for _, line := range bytes.Split(journal, []byte{'\n'}) {
		var event agent.Event
		if json.Unmarshal(line, &event) != nil || event.Message == nil {
			continue
		}
		for _, block := range event.Message.Content {
			if block.Type == "tool_use" && block.Name == "bash" {
				bash[block.ID] = true
			}
			if block.Type == "tool_result" && bash[block.ToolUseID] && !block.IsError {
				var output string
				if json.Unmarshal(block.Content, &output) == nil && strings.Contains(output, strings.TrimSpace(string(receipt))) {
					return true
				}
			}
		}
	}
	return false
}

func TestOrchestrationDeliveryRejectsCompletionWithoutEvidence(t *testing.T) {
	state := board.State{Graph: board.Graph{Project: board.Project{Status: "completed", OrchestrationVersion: 1}}}
	if len(validateOrchestrationDelivery(state, nil)) < 5 {
		t.Fatal("completion was mistaken for independent review acceptance")
	}
}

func orchestrationDeliveryFixture(t *testing.T) (board.State, map[string][]byte) {
	t.Helper()
	files := map[string][]byte{}
	state := board.State{Graph: board.Graph{Project: board.Project{ID: "p1", Status: "completed", OrchestrationVersion: 1}}}
	body := []byte("{\"dataset\":\"sample-v1\",\"values\":[2,3,5]}\n")
	receipt, err := json.Marshal(map[string]any{"dataset": "sample-v1", "values": []int{2, 3, 5}, "sum": 10, "dataset_sha256": fmt.Sprintf("%x", sha256.Sum256(body))})
	if err != nil {
		t.Fatal(err)
	}
	for i, run := range []string{"producer-a", "producer-b", "review-c"} {
		stepID, factID, workerID := fmt.Sprintf("i%d", i+1), fmt.Sprintf("f%d", i+1), "live@"+run
		step := board.Step{ID: stepID, Status: "completed", SupportValid: true, Worker: &workerID, Result: &factID}
		var rawEvidence [][]byte
		switch i {
		case 0:
			rawEvidence = [][]byte{[]byte(`{"dataset":"sample-v1","reported_sum":10,"report":"a"}`)}
		case 1:
			report := `{"dataset":"sample-v1","reported_sum":11,"report":"b"}`
			rawEvidence = [][]byte{[]byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(report), report))}
		case 2:
			step.DisputeID, step.DependsOn = "d1", []string{"i1", "i2"}
			rawEvidence = [][]byte{body, receipt}
		}
		fact := board.FactRecord{ID: factID, Description: "Retained observation " + factID, Scope: "sample-v1", ObservedAt: "2026-09-28T00:00:00Z", Status: "valid", RunID: workerID, SourceStepID: stepID}
		for _, raw := range rawEvidence {
			fact.Evidence = append(fact.Evidence, retainOrchestrationEvidence(run, raw, files))
		}
		status := "verified"
		if i == 1 {
			status = "refuted"
		}
		state.Steps = append(state.Steps, step)
		state.FactRecords = append(state.FactRecords, fact)
		state.Candidates = append(state.Candidates, board.Candidate{ID: fmt.Sprintf("c%d", i+1), Claim: orchestrationClaim, Scope: "sample-v1", Status: status, Sources: []string{factID}, Evidence: fact.Evidence, RunID: workerID, SourceStepID: stepID})
		job := worker.Job{RunID: run, Kind: "explore", GraphRPC: true, ResultContractVersion: 2, Graph: state.Graph, Intent: &board.Intent{ID: stepID}}
		if i == 2 {
			job.DependencyResults = []board.DependencyResult{{StepID: "i1", FactID: "f1", RunID: "producer-a"}, {StepID: "i2", FactID: "f2", RunID: "producer-b"}}
		}
		base := "/workspace/.pwnmesh/runs/" + run + "/"
		files[base+"job.json"], _ = json.Marshal(job)
		files[base+"session.json"], _ = json.Marshal(map[string]any{"run_id": run, "identity": map[string]string{"project_id": "p1", "step_id": stepID, "run_id": run}, "result": worker.Result{Status: "success", Text: fmt.Sprintf(`{"accepted":true,"outcome":"completed","data":{"fact_id":%q}}`, factID)}})
	}
	content, _ := json.Marshal(string(receipt) + "\n")
	for _, message := range []agent.Message{
		{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "calc", Name: "bash", Input: json.RawMessage(`{"command":"python3 calculate.py"}`)}}},
		{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "calc", Content: content}}},
	} {
		raw, _ := json.Marshal(agent.Event{Type: "message_end", Message: &message})
		files["/workspace/.pwnmesh/runs/review-c/events.jsonl"] = append(files["/workspace/.pwnmesh/runs/review-c/events.jsonl"], append(raw, '\n')...)
	}
	state.Disputes = []board.Dispute{{ID: "d1", FindingID: "finding1", Status: "resolved", CandidateIDs: []string{"c1", "c2", "c3"}, ProducerRunIDs: []string{"live@producer-a", "live@producer-b"}, ReviewStepIDs: []string{"i3"}, ReviewFactIDs: []string{"f3"}}}
	state.Findings = []board.Finding{{ID: "finding1", DisputeID: "d1", Claim: orchestrationClaim, Scope: "sample-v1", Status: "verified", SupportValid: true, Sources: []string{"f3"}}}
	return state, files
}

func retainOrchestrationEvidence(run string, raw []byte, files map[string][]byte) board.EvidenceRef {
	name := fmt.Sprintf("/workspace/.pwnmesh/runs/%s/evidence/%x.raw", run, sha256.Sum256(raw))
	files[name] = raw
	return board.EvidenceRef{RunID: run, Path: name, Excerpt: string(raw)}
}

func setOrchestrationResultText(files map[string][]byte, text string) {
	name := "/workspace/.pwnmesh/runs/review-c/session.json"
	var session map[string]json.RawMessage
	_ = json.Unmarshal(files[name], &session)
	session["result"], _ = json.Marshal(worker.Result{Status: "success", Text: text})
	files[name], _ = json.Marshal(session)
}

func TestOrchestrationReviewCandidateAllowsOnlyBoundReportContext(t *testing.T) {
	for _, test := range []struct {
		name   string
		pass   bool
		change func(*board.State, map[string][]byte)
	}{
		{"independent_review_only", true, func(s *board.State, _ map[string][]byte) { s.Candidates[2].Sources = []string{"f3"} }},
		{"both_original_reports", true, func(_ *board.State, _ map[string][]byte) {}},
		{"one_original_report", true, func(s *board.State, _ map[string][]byte) { s.Candidates[2].Sources = []string{"f3", "f1"} }},
		{"unrelated_file", true, func(_ *board.State, files map[string][]byte) {
			files["/workspace/irrelevant.json"] = []byte(`{"sum":999}`)
		}},
		{"only_original_reports", false, func(s *board.State, _ map[string][]byte) { s.Candidates[2].Sources = []string{"f1", "f2"} }},
		{"missing_new_source", false, func(s *board.State, _ map[string][]byte) { s.Candidates[2].Sources = []string{"missing", "f1", "f2"} }},
		{"unbound_fact_from_same_dependency", false, func(s *board.State, _ map[string][]byte) {
			other := s.FactRecords[0]
			other.ID = "unbound-context"
			s.FactRecords = append(s.FactRecords, other)
			s.Candidates[2].Sources = append(s.Candidates[2].Sources, other.ID)
		}},
		{"wrong_review_fact_run", false, func(s *board.State, _ map[string][]byte) { s.FactRecords[2].RunID = "live@producer-a" }},
		{"wrong_review_fact_step", false, func(s *board.State, _ map[string][]byte) { s.FactRecords[2].SourceStepID = "i1" }},
		{"wrong_candidate_run", false, func(s *board.State, _ map[string][]byte) { s.Candidates[2].RunID = "live@producer-a" }},
		{"missing_candidate_dataset", false, func(s *board.State, _ map[string][]byte) { s.Candidates[2].Evidence = s.Candidates[2].Evidence[1:] }},
		{"missing_candidate_calculation", false, func(s *board.State, _ map[string][]byte) { s.Candidates[2].Evidence = s.Candidates[2].Evidence[:1] }},
		{"candidate_only_copied_reports", false, func(s *board.State, files map[string][]byte) {
			refs := []board.EvidenceRef{}
			for _, fact := range s.FactRecords[:2] {
				refs = append(refs, retainOrchestrationEvidence("review-c", files[fact.Evidence[0].Path], files))
			}
			s.FactRecords[2].Evidence = append(s.FactRecords[2].Evidence, refs...)
			s.Candidates[2].Evidence = refs
		}},
		{"candidate_other_run_evidence", false, func(s *board.State, _ map[string][]byte) { s.Candidates[2].Evidence = s.FactRecords[0].Evidence }},
		{"unbound_dependency_job", false, func(_ *board.State, files map[string][]byte) {
			name := "/workspace/.pwnmesh/runs/review-c/job.json"
			var job worker.Job
			_ = json.Unmarshal(files[name], &job)
			job.DependencyResults[0].FactID = "unbound-context"
			files[name], _ = json.Marshal(job)
		}},
		{"missing_successful_context_run", false, func(_ *board.State, files map[string][]byte) {
			delete(files, "/workspace/.pwnmesh/runs/producer-a/session.json")
		}},
		{"ordinary_candidate_cannot_borrow_context", false, func(s *board.State, _ map[string][]byte) {
			s.Candidates[0].Sources = append(s.Candidates[0].Sources, "f2")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, files := orchestrationDeliveryFixture(t)
			state.Candidates[2].Sources = []string{"f3", "f1", "f2"}
			test.change(&state, files)
			failures := validateOrchestrationDelivery(state, files)
			if (len(failures) == 0) != test.pass {
				t.Fatalf("pass=%v failures=%v", test.pass, failures)
			}
		})
	}
}

func TestOrchestrationDeliveryInlineResult(t *testing.T) {
	for _, field := range []string{"", "description", "scope", "observed_at", "evidence"} {
		t.Run("changed_"+field, func(t *testing.T) {
			state, files := orchestrationDeliveryFixture(t)
			fact := state.FactRecords[2]
			// Server normalization preserves semantic timestamps and trims the
			// two text fields. Different JSON formatting is not a changed result.
			inline := map[string]any{"description": " " + fact.Description + " ", "scope": " " + fact.Scope + " ", "observed_at": "2026-09-28T08:00:00+08:00", "evidence": fact.Evidence}
			switch field {
			case "description", "scope":
				inline[field] = "different"
			case "observed_at":
				inline[field] = "2026-09-28T00:00:01Z"
			case "evidence":
				inline[field] = fact.Evidence[:1]
			}
			text, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": inline}})
			setOrchestrationResultText(files, string(text))
			failures := validateOrchestrationDelivery(state, files)
			if (len(failures) == 0) != (field == "") {
				t.Fatalf("changed field %q: %v", field, failures)
			}
		})
	}
}

func TestOrchestrationDeliveryRejectsBrokenProofChain(t *testing.T) {
	replaceReview := func(state *board.State, files map[string][]byte, index int, raw []byte) {
		e := retainOrchestrationEvidence("review-c", raw, files)
		state.FactRecords[2].Evidence[index] = e
		state.Candidates[2].Evidence[index] = e
	}
	for _, test := range []struct {
		name   string
		change func(*board.State, map[string][]byte)
	}{
		{"wrong_claim", func(s *board.State, _ map[string][]byte) { s.Candidates[0].Claim = "Another claim" }},
		{"wrong_scope", func(s *board.State, _ map[string][]byte) { s.Candidates[1].Scope = "other" }},
		{"candidate_stale_generation", func(s *board.State, _ map[string][]byte) { s.Candidates[2].Generation++ }},
		{"step_unsupported", func(s *board.State, _ map[string][]byte) { s.Steps[2].SupportValid = false }},
		{"result_fact_unsupported", func(s *board.State, _ map[string][]byte) { s.FactRecords[2].SupportInvalid = true }},
		{"candidate_source_unsupported", func(s *board.State, _ map[string][]byte) { s.FactRecords[0].SupportInvalid = true }},
		{"candidate_unrelated_evidence", func(s *board.State, f map[string][]byte) {
			s.Candidates[0].Evidence = []board.EvidenceRef{retainOrchestrationEvidence("producer-a", []byte("unrelated observation"), f)}
		}},
		{"candidate_missing_source", func(s *board.State, _ map[string][]byte) {
			s.Candidates[0].Sources = append(s.Candidates[0].Sources, "missing")
		}},
		{"same_producer_step", func(s *board.State, _ map[string][]byte) {
			s.Candidates[1].SourceStepID = "i1"
			s.FactRecords[1].SourceStepID = "i1"
		}},
		{"same_producer_run", func(s *board.State, _ map[string][]byte) {
			s.Candidates[1].RunID = "live@producer-a"
			s.FactRecords[1].RunID = "live@producer-a"
		}},
		{"wrong_raw_report", func(s *board.State, f map[string][]byte) {
			e := retainOrchestrationEvidence("producer-b", []byte(`{"dataset":"sample-v1","reported_sum":10,"report":"b"}`), f)
			s.FactRecords[1].Evidence = []board.EvidenceRef{e}
			s.Candidates[1].Evidence = []board.EvidenceRef{e}
		}},
		{"tampered_archive", func(s *board.State, f map[string][]byte) { f[s.FactRecords[0].Evidence[0].Path] = []byte("tampered") }},
		{"initial_dependency", func(s *board.State, _ map[string][]byte) { s.Steps[1].DependsOn = []string{"i1"} }},
		{"missing_dependency", func(s *board.State, _ map[string][]byte) { s.Steps[2].DependsOn = []string{"i1"} }},
		{"wrong_dependency_binding", func(_ *board.State, f map[string][]byte) {
			p := "/workspace/.pwnmesh/runs/review-c/job.json"
			var j worker.Job
			_ = json.Unmarshal(f[p], &j)
			j.DependencyResults[1].RunID = "producer-a"
			f[p], _ = json.Marshal(j)
		}},
		{"job_legacy_mode", func(_ *board.State, f map[string][]byte) {
			p := "/workspace/.pwnmesh/runs/review-c/job.json"
			var j worker.Job
			_ = json.Unmarshal(f[p], &j)
			j.Graph.Project.OrchestrationVersion = 0
			f[p], _ = json.Marshal(j)
		}},
		{"job_stale_generation", func(_ *board.State, f map[string][]byte) {
			p := "/workspace/.pwnmesh/runs/review-c/job.json"
			var j worker.Job
			_ = json.Unmarshal(f[p], &j)
			j.Graph.Project.Generation++
			f[p], _ = json.Marshal(j)
		}},
		{"session_empty_result", func(_ *board.State, f map[string][]byte) { setOrchestrationResultText(f, "") }},
		{"session_wrong_fact_id", func(_ *board.State, f map[string][]byte) {
			setOrchestrationResultText(f, `{"accepted":true,"outcome":"completed","data":{"fact_id":"f1"}}`)
		}},
		{"session_continue", func(_ *board.State, f map[string][]byte) {
			setOrchestrationResultText(f, `{"accepted":true,"outcome":"continue","reason":"not finished"}`)
		}},
		{"session_incomplete", func(_ *board.State, f map[string][]byte) {
			setOrchestrationResultText(f, `{"accepted":true,"outcome":"incomplete","reason":"blocked"}`)
		}},
		{"session_rejected", func(_ *board.State, f map[string][]byte) {
			setOrchestrationResultText(f, `{"accepted":false,"reason":"cannot finish"}`)
		}},
		{"review_failed", func(_ *board.State, f map[string][]byte) {
			p := "/workspace/.pwnmesh/runs/review-c/session.json"
			f[p] = bytes.ReplaceAll(f[p], []byte(`"success"`), []byte(`"failed"`))
		}},
		{"wrong_session_identity", func(_ *board.State, f map[string][]byte) {
			p := "/workspace/.pwnmesh/runs/review-c/session.json"
			f[p] = bytes.ReplaceAll(f[p], []byte(`"step_id":"i3"`), []byte(`"step_id":"i1"`))
		}},
		{"missing_successful_run", func(_ *board.State, f map[string][]byte) { delete(f, "/workspace/.pwnmesh/runs/review-c/session.json") }},
		{"producer_review", func(s *board.State, _ map[string][]byte) {
			s.Disputes[0].ProducerRunIDs = append(s.Disputes[0].ProducerRunIDs, "live@review-c")
		}},
		{"dispute_original_fact", func(s *board.State, _ map[string][]byte) { s.Disputes[0].ReviewFactIDs = []string{"f1"} }},
		{"finding_original_fact", func(s *board.State, _ map[string][]byte) { s.Findings[0].Sources = []string{"f1"} }},
		{"unlinked_review_candidate", func(s *board.State, _ map[string][]byte) { s.Candidates[2].Sources = []string{"f1"} }},
		{"unlinked_finding", func(s *board.State, _ map[string][]byte) { s.Findings[0].DisputeID = "other" }},
		{"number_in_prose", func(s *board.State, f map[string][]byte) { replaceReview(s, f, 0, []byte("sample-v1 equals 10")) }},
		{"different_dataset_same_sum", func(s *board.State, f map[string][]byte) {
			replaceReview(s, f, 0, []byte(`{"dataset":"sample-v1","values":[1,4,5]}`))
		}},
		{"wrong_calculation", func(s *board.State, f map[string][]byte) {
			old := f[s.FactRecords[2].Evidence[1].Path]
			replaceReview(s, f, 1, bytes.ReplaceAll(old, []byte(`"sum":10`), []byte(`"sum":11`)))
		}},
		{"wrong_dataset_hash", func(s *board.State, f map[string][]byte) {
			replaceReview(s, f, 1, []byte(`{"dataset":"sample-v1","values":[2,3,5],"sum":10,"dataset_sha256":"wrong"}`))
		}},
		{"receipt_only", func(s *board.State, _ map[string][]byte) { s.FactRecords[2].Evidence = s.FactRecords[2].Evidence[1:] }},
		{"no_command_output", func(_ *board.State, f map[string][]byte) { delete(f, "/workspace/.pwnmesh/runs/review-c/events.jsonl") }},
		{"failed_command_output", func(_ *board.State, f map[string][]byte) {
			p := "/workspace/.pwnmesh/runs/review-c/events.jsonl"
			f[p] = bytes.ReplaceAll(f[p], []byte(`"type":"tool_result"`), []byte(`"type":"tool_result","is_error":true`))
		}},
		{"graph_text_is_not_execution", func(_ *board.State, f map[string][]byte) {
			p := "/workspace/.pwnmesh/runs/review-c/events.jsonl"
			f[p] = bytes.ReplaceAll(f[p], []byte(`"name":"bash"`), []byte(`"name":"graph_read"`))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, files := orchestrationDeliveryFixture(t)
			test.change(&state, files)
			if len(validateOrchestrationDelivery(state, files)) == 0 {
				t.Fatal("accepted broken evidence/provenance chain")
			}
		})
	}
}

func TestOrchestrationEvidenceHTTPFraming(t *testing.T) {
	body := `{"dataset":"sample-v1","values":[2,3,5]}`
	for _, raw := range []string{
		"HTTP/1.1 500 Error\r\nContent-Length: 40\r\n\r\n" + body,
		"HTTP/1.1 200 OK\r\nContent-Length: 400\r\n\r\n" + body,
		fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s extra", len(body), body),
		"explanation: " + body,
	} {
		if orchestrationJSONBody([]byte(raw)) != nil {
			t.Fatalf("accepted invalid raw response %q", raw)
		}
	}
}
