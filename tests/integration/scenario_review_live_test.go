//go:build linux

package integration

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

const scenarioReviewClaim = "The vulnerable invoice implementation permits foreign-owner access while the fixed control rejects it"
const scenarioReviewScope = "synthetic ownership fixture"

type scenarioReviewOracle struct {
	Source []byte
	Nonce  string
	Bodies map[string][]byte
}

func validateScenarioReview(state board.State, files map[string][]byte, scenario string, bundle map[string][]byte, fixture *scenarioSkillFixture) []string {
	failures := validateScenarioSkillSteps(state, files, scenario, bundle, fixture, 2)
	failures = append(failures, validateScenarioReviewFetches(fixture, scenario)...)
	failures = append(failures, validateScenarioReviewCuration(state)...)
	return append(failures, validateScenarioReviewCurationInput(state, files)...)
}

func validateScenarioReviewFetches(fixture *scenarioSkillFixture, scenario string) []string {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	paths := []string{"/audit.py"}
	if scenario == "pentest" {
		paths = nil
		for path := range fixture.bodies {
			paths = append(paths, path)
		}
	}
	var failures []string
	for _, path := range paths {
		if fixture.reads[path] < 2 {
			failures = append(failures, "missing repeated original fetch: "+path)
		}
	}
	return failures
}

func validateScenarioReviewCurationInput(state board.State, files map[string][]byte) []string {
	var job worker.Job
	run := orchestrationRunID(state.Curation.RunID)
	decoded := json.Unmarshal(files["/workspace/.pwnmesh/runs/"+run+"/job.json"], &job) == nil
	input := retainedCurationInput(job, files)
	if !decoded || job.Kind != "curate" || job.RunID != run || job.Graph.Project.ID != state.Graph.Project.ID || input == nil || input.Graph.Project.ID != state.Graph.Project.ID || input.Graph.Project.Generation != state.Graph.Project.Generation || input.Revision != state.Curation.ThroughRevision {
		return []string{"missing immutable final curation input"}
	}
	var failures []string
	for _, candidate := range state.Candidates {
		if !slices.ContainsFunc(input.Candidates, func(original board.Candidate) bool {
			return reflect.DeepEqual(original, candidate)
		}) {
			failures = append(failures, "final curator did not receive both producers")
		}
	}
	return failures
}

func validateScenarioReviewCuration(state board.State) []string {
	failures := []string{}
	check := func(ok bool, message string) {
		if !ok {
			failures = append(failures, message)
		}
	}
	facts := map[string]board.FactRecord{}
	results := map[string]board.Step{}
	for _, fact := range state.FactRecords {
		facts[fact.ID] = fact
	}
	for _, step := range state.Steps {
		if staleRepairCompletionMarker(state, step) {
			continue
		}
		check(len(step.DependsOn) == 0, "ordinary producer Step has dependencies: "+step.ID)
		if step.Result != nil {
			results[*step.Result] = step
		}
	}
	paths := map[string]string{}
	for id := range results {
		for _, evidence := range facts[id].Evidence {
			previous, exists := paths[evidence.Path]
			check(!exists || previous == id, "independent producer results reused evidence path: "+evidence.Path)
			paths[evidence.Path] = id
		}
	}
	check(len(results) == 2 && len(state.Candidates) == 2, "expected two distinct producer results and candidates")
	check(len(state.Findings) == 1 && len(state.Disputes) == 0, "expected one finding without an artificial dispute")
	if len(state.Findings) != 1 {
		return failures
	}
	finding := state.Findings[0]
	check(finding.Claim == scenarioReviewClaim && finding.Scope == scenarioReviewScope && finding.Status == "verified" && finding.SupportValid, "finding is unrelated or unsupported")
	check(len(finding.Sources) == 2 && len(finding.CandidateIDs) == 2, "finding dropped an independent producer")
	runs := map[string]bool{}
	for _, candidate := range state.Candidates {
		check(candidate.Claim == scenarioReviewClaim && candidate.Scope == scenarioReviewScope && candidate.Status == "verified", "candidate changed claim, scope or verdict")
		check(candidate.Generation == state.Graph.Project.Generation && candidate.Revision > 0 && candidate.Revision <= state.Curation.ThroughRevision && candidate.Revision <= finding.CuratedRevision, "candidate is outside the curated generation/revision")
		check(orchestrationCandidateSources(candidate, facts), "candidate lost its original evidence provenance")
		check(len(candidate.Sources) == 1 && slices.Contains(finding.CandidateIDs, candidate.ID), "candidate is missing from the finding")
		if len(candidate.Sources) != 1 {
			continue
		}
		id := candidate.Sources[0]
		step, exists := results[id]
		fact := facts[id]
		check(exists && step.ID == candidate.SourceStepID && step.Worker != nil && *step.Worker == candidate.RunID && candidate.RunID != "", "candidate does not belong to its successful producer")
		runs[candidate.RunID] = true
		check(slices.Contains(finding.Sources, id) && len(candidate.Evidence) == len(fact.Evidence) && len(fact.Evidence) > 0, "candidate/finding lost result support")
		for _, evidence := range fact.Evidence {
			check(curationEvidenceIdentity(candidate.Evidence, evidence) && curationEvidenceIdentity(finding.Evidence, evidence), "finding lost original target/control evidence")
		}
	}
	check(len(runs) == 2, "the two judgments reused one producer run")
	for _, evidence := range finding.Evidence {
		check(slices.ContainsFunc(finding.Sources, func(id string) bool { return curationEvidenceIdentity(facts[id].Evidence, evidence) }), "finding invented evidence outside its sources")
	}
	return failures
}

func TestScenarioReviewCurationRejectsDroppedProducer(t *testing.T) {
	makeState := func() board.State {
		state := board.State{Graph: board.Graph{Project: board.Project{Generation: 1}}, Curation: board.CurationProgress{ThroughRevision: 3}}
		finding := board.Finding{Claim: scenarioReviewClaim, Scope: scenarioReviewScope, Status: "verified", SupportValid: true, CuratedRevision: 3}
		for _, id := range []string{"a", "b"} {
			run, factID := "run-"+id, "fact-"+id
			evidence := []board.EvidenceRef{{RunID: run, Path: "/workspace/" + id}}
			state.Steps = append(state.Steps, board.Step{ID: id, Worker: &run, Result: &factID})
			state.FactRecords = append(state.FactRecords, board.FactRecord{ID: factID, RunID: run, SourceStepID: id, Status: "valid", Evidence: evidence})
			state.Candidates = append(state.Candidates, board.Candidate{ID: id, Claim: scenarioReviewClaim, Scope: scenarioReviewScope, Status: "verified", Sources: []string{factID}, Evidence: evidence, RunID: run, SourceStepID: id, Generation: 1, Revision: 2})
			finding.CandidateIDs = append(finding.CandidateIDs, id)
			finding.Sources = append(finding.Sources, factID)
			finding.Evidence = append(finding.Evidence, evidence...)
		}
		state.Findings = []board.Finding{finding}
		return state
	}
	if failures := validateScenarioReviewCuration(makeState()); len(failures) != 0 {
		t.Fatal(failures)
	}
	for index, mutate := range []func(*board.State){
		func(s *board.State) { s.Findings[0].Sources = s.Findings[0].Sources[:1] },
		func(s *board.State) { s.Findings[0].Evidence = s.Findings[0].Evidence[:1] },
		func(s *board.State) { s.Candidates[1].RunID = s.Candidates[0].RunID },
		func(s *board.State) { s.Curation.ThroughRevision = 1 },
		func(s *board.State) { s.Findings[0].SupportValid = false },
		func(s *board.State) { s.Steps[1].DependsOn = []string{s.Steps[0].ID} },
		func(s *board.State) {
			shared := s.FactRecords[0].Evidence[0].Path
			s.FactRecords[1].Evidence[0].Path = shared
			s.Candidates[1].Evidence[0].Path = shared
			s.Findings[0].Evidence[1].Path = shared
		},
	} {
		state := makeState()
		mutate(&state)
		if len(validateScenarioReviewCuration(state)) == 0 {
			t.Errorf("accepted incomplete or mismatched independent support: mutation %d", index)
		}
	}
}

func TestScenarioReviewFetchesRejectInsufficientRequests(t *testing.T) {
	for _, scenario := range []string{"audit", "pentest"} {
		t.Run(scenario, func(t *testing.T) {
			paths := []string{"/audit.py"}
			if scenario == "pentest" {
				paths = []string{"/vulnerable/invoices/1001", "/vulnerable/invoices/1002", "/fixed/invoices/1001", "/fixed/invoices/1002"}
			}
			fixture := &scenarioSkillFixture{reads: map[string]int{}, bodies: map[string][]byte{}}
			for _, path := range paths {
				fixture.reads[path], fixture.bodies[path] = 2, []byte("synthetic")
			}
			if failures := validateScenarioReviewFetches(fixture, scenario); len(failures) != 0 {
				t.Fatal(failures)
			}
			for _, path := range paths {
				for _, count := range []int{0, 1} {
					fixture.reads[path] = count
					if len(validateScenarioReviewFetches(fixture, scenario)) == 0 {
						t.Errorf("accepted %d requests for %s", count, path)
					}
				}
				fixture.reads[path] = 2
			}
		})
	}
}

func TestScenarioReviewCurationInputRejectsCorruptOrIncompleteSnapshots(t *testing.T) {
	fixture := func(t *testing.T) (board.State, map[string][]byte, worker.Job) {
		t.Helper()
		state, files := curationSupportDeliveryFixture(t)
		path := "/workspace/.pwnmesh/runs/curator-run/job.json"
		var job worker.Job
		if err := json.Unmarshal(files[path], &job); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(job.State)
		job.InputSnapshot = &board.InputSnapshot{Version: 1, ID: fmt.Sprintf("%x", sha256.Sum256(raw)), ProjectID: job.Graph.Project.ID, Generation: job.Graph.Project.Generation, Revision: job.State.Revision, StateVersion: board.DecisionStateVersion(*job.State)}
		job.Graph, job.State = board.Graph{Project: job.Graph.Project}, nil
		files[curationSnapshotKey(job.InputSnapshot.ID)] = raw
		files[path], _ = json.Marshal(job)
		return state, files, job
	}
	state, files, _ := fixture(t)
	if failures := validateScenarioReviewCurationInput(state, files); len(failures) != 0 {
		t.Fatal(failures)
	}
	for _, mode := range []string{"missing_snapshot", "changed_snapshot", "changed_candidate", "wrong_job", "wrong_revision"} {
		t.Run(mode, func(t *testing.T) {
			state, files, job := fixture(t)
			key := curationSnapshotKey(job.InputSnapshot.ID)
			switch mode {
			case "missing_snapshot":
				delete(files, key)
			case "changed_snapshot":
				files[key] = append(files[key], ' ')
			case "changed_candidate":
				state.Candidates[1].Reason = "Changed after the curator received its input"
			case "wrong_job":
				job.Kind = "explore"
				files["/workspace/.pwnmesh/runs/curator-run/job.json"], _ = json.Marshal(job)
			case "wrong_revision":
				state.Curation.ThroughRevision++
			}
			if len(validateScenarioReviewCurationInput(state, files)) == 0 {
				t.Fatal("accepted corrupt or incomplete final curation input")
			}
		})
	}
}
