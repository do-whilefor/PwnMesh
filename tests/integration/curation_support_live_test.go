//go:build linux

package integration

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

const curationSupportClaim = "The sum of sample-v1 values is 10"

func validateCurationSupportDelivery(state board.State, files map[string][]byte) []string {
	// Reuse the independent byte, calculation, successful execution, relation,
	// immutable curator input and completion checks. Candidate rules are checked
	// below against the original state; never mutate the supplied observation.
	observations := state
	observations.Candidates = nil
	failures := validateCurationRelationsDelivery(observations, files)
	check := func(ok bool, message string) {
		if !ok {
			failures = append(failures, message)
		}
	}
	var old, fresh board.FactRecord
	for _, fact := range state.FactRecords {
		if fact.Description == curationOldClaim {
			old = fact
		}
		if fact.Description == curationNewClaim {
			fresh = fact
		}
	}
	check(len(state.Candidates) == 2, "expected both retained tentative and verified candidates")
	var tentative, verified board.Candidate
	for _, candidate := range state.Candidates {
		check(candidate.ID != "" && candidate.Claim == curationSupportClaim && candidate.Scope == "sample-v1", "candidate changed the common claim/scope")
		check(candidate.Generation == state.Graph.Project.Generation && candidate.CreatedAt != "" && candidate.Revision > 0 && candidate.Revision <= state.Curation.ThroughRevision && candidate.Reason != "", "candidate identity or curation boundary is missing")
		var source board.FactRecord
		switch candidate.Status {
		case "candidate":
			tentative, source = candidate, old
		case "verified":
			verified, source = candidate, fresh
		default:
			check(false, "unexpected opposing or invalid candidate verdict")
		}
		check(source.ID != "" && slices.Equal(candidate.Sources, []string{source.ID}) && candidate.RunID == source.RunID && candidate.SourceStepID == source.SourceStepID, "candidate lost its original source/producer binding")
		check(len(candidate.Evidence) == len(source.Evidence) && len(candidate.Evidence) > 0, "candidate lost explicit retained evidence")
		for _, ref := range candidate.Evidence {
			check(orchestrationEvidenceValid(ref, candidate.RunID, files) && curationEvidenceIdentity(source.Evidence, ref), "candidate evidence does not match its retained source")
		}
		for _, ref := range source.Evidence {
			check(curationEvidenceIdentity(candidate.Evidence, ref), "candidate omitted a required source artifact")
		}
	}
	check(tentative.ID != "" && verified.ID != "" && tentative.ID != verified.ID, "both original candidate judgments were not retained")
	// The relation may have been created before the final candidate arrived.
	// Use the final covered immutable input, not the relation's historical run.
	run := orchestrationRunID(state.Curation.RunID)
	var job worker.Job
	sameProject := func(project board.Project) bool {
		current := state.Graph.Project
		return current.ID != "" && project.ID == current.ID && project.Generation == current.Generation && project.OrchestrationVersion == current.OrchestrationVersion
	}
	decoded := json.Unmarshal(files["/workspace/.pwnmesh/runs/"+run+"/job.json"], &job) == nil
	input := retainedCurationInput(job, files)
	if run != "" && decoded && job.RunID == run && job.Kind == "curate" && sameProject(job.Graph.Project) && input != nil && sameProject(input.Graph.Project) && input.Revision == state.Curation.ThroughRevision {
		check(len(input.Candidates) == len(state.Candidates), "curation dropped historical candidates from its immutable input")
		for _, candidate := range state.Candidates {
			check(slices.ContainsFunc(input.Candidates, func(original board.Candidate) bool { return reflect.DeepEqual(original, candidate) }), "curation changed or fabricated a historical candidate")
		}
	} else {
		check(false, "candidate history lacks the final covered immutable curator input")
	}
	check(len(state.Findings) == 1, "expected one final supported finding")
	if len(state.Findings) == 1 {
		finding := state.Findings[0]
		check(finding.ID != "" && finding.Claim == curationSupportClaim && finding.Scope == "sample-v1" && finding.Status == "verified" && finding.SupportValid && finding.DisputeID == "", "final finding is unrelated, unverified or unsupported")
		check(len(finding.CandidateIDs) == 2 && slices.Contains(finding.CandidateIDs, tentative.ID) && slices.Contains(finding.CandidateIDs, verified.ID), "finding lost historical candidate provenance")
		check(slices.Equal(finding.Sources, []string{fresh.ID}) && state.ValidateFactSources(finding.Sources, true) == nil, "finding retains superseded or unrelated current sources")
		check(len(finding.Evidence) == len(verified.Evidence) && len(finding.Evidence) > 0, "finding lost current calculation evidence")
		for _, ref := range finding.Evidence {
			check(slices.Contains(verified.Evidence, ref) && !slices.Contains(tentative.Evidence, ref) && orchestrationEvidenceValid(ref, fresh.RunID, files), "finding retains superseded or unrelated current evidence")
		}
		for _, ref := range verified.Evidence {
			check(slices.Contains(finding.Evidence, ref), "finding omitted required verified evidence")
		}
		check(finding.CuratedRevision > 0 && finding.CuratedRevision <= state.Curation.ThroughRevision && finding.CuratedRevision >= verified.Revision, "finding was not curated after the fresh candidate")
	}
	return failures
}

func curationEvidenceIdentity(refs []board.EvidenceRef, ref board.EvidenceRef) bool {
	// Selection line bounds can differ while pointing at the same immutable
	// bytes. Ownership and content address establish the retained artifact.
	return slices.ContainsFunc(refs, func(other board.EvidenceRef) bool {
		return other.RunID == ref.RunID && other.Path == ref.Path
	})
}

func curationSupportDeliveryFixture(t *testing.T) (board.State, map[string][]byte) {
	t.Helper()
	state, files := curationRelationsDeliveryFixture(t)
	old, fresh := state.FactRecords[0], state.FactRecords[1]
	for n, fact := range []board.FactRecord{old, fresh} {
		status := "candidate"
		if n == 1 {
			status = "verified"
		}
		state.Candidates = append(state.Candidates, board.Candidate{ID: "candidate-" + fact.ID, Claim: curationSupportClaim, Scope: "sample-v1", Status: status, Sources: []string{fact.ID}, Evidence: slices.Clone(fact.Evidence), Reason: "Preserve the observed interpretation", RunID: fact.RunID, SourceStepID: fact.SourceStepID, Generation: state.Graph.Project.Generation, Revision: int64(n + 1), CreatedAt: fact.ObservedAt})
	}
	state.Findings = []board.Finding{{ID: "current-support", Claim: curationSupportClaim, Scope: "sample-v1", Status: "verified", Sources: []string{fresh.ID}, Evidence: slices.Clone(fresh.Evidence), SupportValid: true, CandidateIDs: []string{state.Candidates[0].ID, state.Candidates[1].ID}, CuratedRevision: state.Curation.ThroughRevision}}
	path := "/workspace/.pwnmesh/runs/curator-run/job.json"
	var job worker.Job
	if err := json.Unmarshal(files[path], &job); err != nil {
		t.Fatal(err)
	}
	input := curationFixtureInput(t, files, job)
	input.Candidates = state.Candidates
	job = freezeCurationFixture(t, files, job, input)
	files[path], _ = json.Marshal(job)
	return state, files
}

func TestCurationSupportDeliveryValidatesRetainedProof(t *testing.T) {
	state, files := curationSupportDeliveryFixture(t)
	if failures := validateCurationSupportDelivery(state, files); len(failures) != 0 {
		t.Fatal(failures)
	}
	t.Run("relation_precedes_final_candidate", func(t *testing.T) {
		state, files := curationSupportDeliveryFixture(t)
		path := "/workspace/.pwnmesh/runs/curator-run/job.json"
		var early worker.Job
		_ = json.Unmarshal(files[path], &early)
		input := curationFixtureInput(t, files, early)
		input.Candidates = input.Candidates[:1]
		early = freezeCurationFixture(t, files, early, input)
		files[path], _ = json.Marshal(early)
		state.Candidates[1].Revision = 4
		state.Curation.ThroughRevision = 4
		state.Curation.RunID = "controlled@later-curator"
		state.Findings[0].CuratedRevision = 4
		input.Revision, input.Candidates = 4, state.Candidates
		later := freezeCurationFixture(t, files, worker.Job{RunID: "later-curator", Kind: "curate", GraphRPC: true, ResultContractVersion: 2, Graph: input.Graph}, input)
		files["/workspace/.pwnmesh/runs/later-curator/job.json"], _ = json.Marshal(later)
		if failures := validateCurationSupportDelivery(state, files); len(failures) != 0 {
			t.Fatal("valid later curation rejected:", failures)
		}
	})
	for _, test := range []struct {
		name   string
		change func(*board.State, map[string][]byte)
	}{
		{"stale_finding_source", func(s *board.State, _ map[string][]byte) {
			s.Findings[0].Sources = append(s.Findings[0].Sources, s.FactRecords[0].ID)
		}},
		{"stale_finding_evidence", func(s *board.State, _ map[string][]byte) { s.Findings[0].Evidence[0] = s.Candidates[0].Evidence[0] }},
		{"duplicate_finding_evidence", func(s *board.State, _ map[string][]byte) { s.Findings[0].Evidence[0] = s.Findings[0].Evidence[1] }},
		{"missing_original_candidate", func(s *board.State, _ map[string][]byte) { s.Candidates = s.Candidates[1:] }},
		{"lost_candidate_history", func(s *board.State, _ map[string][]byte) { s.Findings[0].CandidateIDs = s.Findings[0].CandidateIDs[1:] }},
		{"historical_candidate_rewritten", func(s *board.State, _ map[string][]byte) { s.Candidates[0].Reason = "rewritten during curation" }},
		{"old_candidate_promoted", func(s *board.State, _ map[string][]byte) { s.Candidates[0].Status = "verified" }},
		{"fresh_candidate_not_verified", func(s *board.State, _ map[string][]byte) { s.Candidates[1].Status = "candidate" }},
		{"fresh_candidate_uses_stale_source", func(s *board.State, _ map[string][]byte) { s.Candidates[1].Sources = []string{s.FactRecords[0].ID} }},
		{"fresh_candidate_uses_stale_evidence", func(s *board.State, _ map[string][]byte) { s.Candidates[1].Evidence[0] = s.Candidates[0].Evidence[0] }},
		{"unverified_finding", func(s *board.State, _ map[string][]byte) { s.Findings[0].Status = "candidate" }},
		{"unsupported_finding", func(s *board.State, _ map[string][]byte) { s.Findings[0].SupportValid = false }},
		{"unrelated_finding", func(s *board.State, _ map[string][]byte) { s.Findings[0].Claim = "Some other calculation is correct" }},
		{"premature_finding", func(s *board.State, _ map[string][]byte) { s.Findings[0].CuratedRevision = 1 }},
		{"wrong_final_input_boundary", func(s *board.State, _ map[string][]byte) { s.Curation.ThroughRevision++ }},
		{"lost_immutable_candidates", func(_ *board.State, f map[string][]byte) {
			path := "/workspace/.pwnmesh/runs/curator-run/job.json"
			var job worker.Job
			_ = json.Unmarshal(f[path], &job)
			input := curationFixtureInput(t, f, job)
			input.Candidates = nil
			job = freezeCurationFixture(t, f, job, input)
			f[path], _ = json.Marshal(job)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, files := curationSupportDeliveryFixture(t)
			test.change(&state, files)
			if failures := validateCurationSupportDelivery(state, files); len(failures) == 0 {
				t.Fatal("broken current-support proof was accepted")
			}
		})
	}
	for _, test := range []struct {
		name   string
		change func(*worker.Job, *board.State)
	}{
		{"wrong_curator_run", func(job *worker.Job, _ *board.State) { job.RunID = "other-run" }},
		{"wrong_job_project", func(job *worker.Job, _ *board.State) { job.Graph.Project.ID = "other-project" }},
		{"wrong_input_project", func(_ *worker.Job, input *board.State) { input.Graph.Project.ID = "other-project" }},
		{"wrong_job_generation", func(job *worker.Job, _ *board.State) { job.Graph.Project.Generation++ }},
		{"wrong_input_generation", func(_ *worker.Job, input *board.State) { input.Graph.Project.Generation++ }},
		{"wrong_job_mode", func(job *worker.Job, _ *board.State) { job.Graph.Project.OrchestrationVersion = 0 }},
		{"wrong_input_mode", func(_ *worker.Job, input *board.State) { input.Graph.Project.OrchestrationVersion = 0 }},
		{"dropped_input_candidate", func(_ *worker.Job, input *board.State) {
			extra := input.Candidates[0]
			extra.ID = "third-historical-candidate"
			input.Candidates = append(input.Candidates, extra)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, files := curationSupportDeliveryFixture(t)
			path := "/workspace/.pwnmesh/runs/curator-run/job.json"
			var job worker.Job
			if err := json.Unmarshal(files[path], &job); err != nil {
				t.Fatal(err)
			}
			input := curationFixtureInput(t, files, job)
			test.change(&job, &input)
			job = freezeCurationFixture(t, files, job, input)
			files[path], _ = json.Marshal(job)
			if failures := validateCurationSupportDelivery(state, files); len(failures) == 0 {
				t.Fatal("unbound curator input was accepted")
			}
		})
	}
}
