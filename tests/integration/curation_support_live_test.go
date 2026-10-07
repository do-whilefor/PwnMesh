//go:build linux

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

const curationSupportClaim = "The sum of sample-v1 values is 10"

// The original interpretation is tentative, not an opposing verdict that
// would require independent review. Curation must retain its provenance while
// excluding its superseded premises from the final supported conclusion.
func TestLiveCurationSupportProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_CURATION_SUPPORT_TEST") != "1" {
		t.Skip("opt in to the real model and Docker current-support acceptance")
	}
	origin := fmt.Sprintf(`Exercise current-support curation using ONLY synthetic local files under /workspace. Do not read credentials, install packages, or contact other services.
Authorize exactly one ordinary Step. Its Worker first writes these exact bytes to %s: %q, and this intentionally wrong initial transcription to %s: %q. Publish a fact with exactly this description: %q, scope sample-v1, and evidence from the transcription file. Then publish a candidate with claim %q, scope sample-v1, status candidate (tentative, never refuted or verified), sources containing only that initial fact ID, and explicit evidence from the transcription file. Explain that the unverified sum=11 transcription does not yet establish the common claim.
In the SAME Step, use a deterministic bash command to read every integer from the retained values file, calculate the sum, and compute SHA-256 of its exact bytes. Save AND print a JSON receipt to %s with fields sum (computed, never hardcoded) and dataset_sha256. Publish a fresh fact with exactly this description: %q, scope sample-v1, and evidence from BOTH the unchanged input and the calculation receipt. Publish a second candidate with the EXACT SAME claim %q and scope sample-v1, status verified, sources containing only the fresh fact ID, and explicit evidence from BOTH input and receipt. Finish the Step with the fresh fact as its result. Preserve all original files, facts and candidates.
The blackboard Agent must atomically submit the supersedes relation from the fresh fact to the initial fact and one group containing BOTH candidate IDs, status verified, and a reason explaining the new calculation and superseded premise. Keep both historical CandidateIDs. The final Finding's current Sources and Evidence must contain only the valid fresh calculation support. If an early curation ran before both candidates were available, its later update must preserve the same final history and effective support. This is tentative-to-verified correction, with no opposing verified/refuted verdict, so no dispute or independent-review Step is needed. The main Agent may complete only after the supported verified Finding and relation are curated, citing the fresh fact. No report or extra Step is needed.`, curationValuesPath, curationValues, curationOriginalPath, curationOriginal, curationOldClaim, curationSupportClaim, curationCorrectedPath, curationNewClaim, curationSupportClaim)
	runObservedProject(t, "Retain historical candidates with fresh verified support", origin, "Recalculate 2+3+5 from retained bytes, supersede the mistaken interpretation, retain both candidate judgments, and complete with a verified Finding supported only by the fresh calculation.", "current_curation_support_with_retained_candidates", validateCurationSupportDelivery, 1)
}

func TestDockerCurationSupport(t *testing.T) {
	runDockerCurationRelations(t, true)
}

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
	job.State.Candidates = state.Candidates
	job.RunID, job.Graph = "curator-run", state.Graph
	job.State.Graph = state.Graph
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
		early.State.Candidates = early.State.Candidates[:1]
		files[path], _ = json.Marshal(early)
		state.Candidates[1].Revision = 4
		state.Curation.ThroughRevision = 4
		state.Curation.RunID = "controlled@later-curator"
		state.Findings[0].CuratedRevision = 4
		files["/workspace/.pwnmesh/runs/later-curator/job.json"], _ = json.Marshal(worker.Job{RunID: "later-curator", Kind: "curate", Graph: state.Graph, State: &board.State{Graph: state.Graph, Revision: 4, Candidates: state.Candidates}})
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
			job.State.Candidates = nil
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
		change func(*worker.Job)
	}{
		{"wrong_curator_run", func(job *worker.Job) { job.RunID = "other-run" }},
		{"wrong_job_project", func(job *worker.Job) { job.Graph.Project.ID = "other-project" }},
		{"wrong_input_project", func(job *worker.Job) { job.State.Graph.Project.ID = "other-project" }},
		{"wrong_job_generation", func(job *worker.Job) { job.Graph.Project.Generation++ }},
		{"wrong_input_generation", func(job *worker.Job) { job.State.Graph.Project.Generation++ }},
		{"wrong_job_mode", func(job *worker.Job) { job.Graph.Project.OrchestrationVersion = 0 }},
		{"wrong_input_mode", func(job *worker.Job) { job.State.Graph.Project.OrchestrationVersion = 0 }},
		{"dropped_input_candidate", func(job *worker.Job) {
			extra := job.State.Candidates[0]
			extra.ID = "third-historical-candidate"
			job.State.Candidates = append(job.State.Candidates, extra)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, files := curationSupportDeliveryFixture(t)
			path := "/workspace/.pwnmesh/runs/curator-run/job.json"
			var job worker.Job
			if err := json.Unmarshal(files[path], &job); err != nil {
				t.Fatal(err)
			}
			test.change(&job)
			files[path], _ = json.Marshal(job)
			if failures := validateCurationSupportDelivery(state, files); len(failures) == 0 {
				t.Fatal("unbound curator input was accepted")
			}
		})
	}
}
