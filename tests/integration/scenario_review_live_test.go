//go:build linux

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// Two independent producers exercise scenario guidance, evidence retention,
// candidate publication, concurrent scheduling and final curation together.
func TestLiveScenarioSkillsReviewProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_SCENARIO_REVIEW_TEST") != "1" {
		t.Skip("opt in to real model scenario Skills and multi-producer curation")
	}
	for _, scenario := range []string{"audit", "pentest"} {
		t.Run(scenario, func(t *testing.T) {
			output := os.Getenv("PWNMESH_LIVE_OUTPUT")
			if output == "" {
				t.Fatal("PWNMESH_LIVE_OUTPUT is required")
			}
			t.Setenv("PWNMESH_LIVE_OUTPUT", filepath.Join(output, scenario))
			fixture, server := newScenarioSkillFixture()
			defer server.Close()
			if err := os.MkdirAll(filepath.Join(output, scenario), 0700); err != nil {
				t.Fatal(err)
			}
			if err := saveLiveJSON(filepath.Join(output, scenario, "fixture-oracle.json"), scenarioReviewOracle{fixture.source, fixture.nonce, fixture.bodies}); err != nil {
				t.Fatal(err)
			}
			origin := fmt.Sprintf(`Use only synthetic local resources at %s and Step-private directories under /workspace. Do not install packages, read credentials or contact other targets.
Authorize exactly two independent ordinary Steps in one initial decision, both from origin and with no depends_on. Each Step must independently perform the entire ownership check below, using its OWN directory and freshly fetched evidence. Never reuse the other producer's files or result. This is two reproductions of the same bounded claim; do not split target and control across Steps.
Each Worker publishes one evidence-backed fact containing its complete target/control observations, then one candidate with claim exactly %q, scope exactly %q, status verified if its observations support the claim, sources containing ONLY its own fact ID, and explicit evidence covering ALL files from that fact. Finish using that existing fact ID. No extra candidate or report-writing Step is needed.
The blackboard Agent merges BOTH candidates into one supported verified Finding while retaining both candidate IDs, both independent source facts, and their original evidence. If early curation sees only one candidate, update the same finding once the second arrives. The main Agent completes only after both Steps succeed, both candidates are curated, and the Finding retains both sources. Cite both result fact IDs when completing. Do not complete from the first producer alone.
Required procedure for EACH Step: %s`, server.URL, scenarioReviewClaim, scenarioReviewScope, scenarioOwnershipTask(scenario))
			bundle := scenarioSkillBundle(t, scenario)
			runObservedScenarioProject(t, "Independent "+scenario+" ownership reproductions", origin,
				"Independently reproduce the target/control comparison twice and complete with one curated finding retaining both producers' evidence.",
				scenario, "scenario_skills_multi_producer_curation", func(state board.State, files map[string][]byte) []string {
					fixture.mu.Lock()
					err := saveLiveJSON(filepath.Join(output, scenario, "fixture-observations.json"), map[string]any{"statuses": fixture.seen, "requests": fixture.reads})
					var fetchFailures []string
					paths := []string{"/audit.py"}
					if scenario == "pentest" {
						paths = nil
						for path := range fixture.bodies {
							paths = append(paths, path)
						}
					}
					for _, path := range paths {
						if fixture.reads[path] < 2 {
							fetchFailures = append(fetchFailures, "missing repeated original fetch: "+path)
						}
					}
					fixture.mu.Unlock()
					if err != nil {
						t.Error(err)
					}
					failures := validateScenarioSkillSteps(state, files, scenario, bundle, fixture, 2)
					failures = append(failures, fetchFailures...)
					failures = append(failures, validateScenarioReviewCuration(state)...)
					var job worker.Job
					run := orchestrationRunID(state.Curation.RunID)
					decoded := json.Unmarshal(files["/workspace/.pwnmesh/runs/"+run+"/job.json"], &job) == nil
					input := retainedCurationInput(job, files)
					if !decoded || job.Kind != "curate" || job.RunID != run || job.Graph.Project.ID != state.Graph.Project.ID || input == nil || input.Graph.Project.ID != state.Graph.Project.ID || input.Graph.Project.Generation != state.Graph.Project.Generation || input.Revision != state.Curation.ThroughRevision {
						failures = append(failures, "missing immutable final curation input")
					} else {
						for _, candidate := range state.Candidates {
							if !slices.ContainsFunc(input.Candidates, func(original board.Candidate) bool {
								return reflect.DeepEqual(original, candidate)
							}) {
								failures = append(failures, "final curator did not receive both producers")
							}
						}
					}
					return failures
				}, 1)
		})
	}
}

// Recheck producer separation, curation and actual successful model overlap
// from retained evidence, without another model call or changing that evidence.
func TestRetainedScenarioSkillsReviewProject(t *testing.T) {
	dir := os.Getenv("PWNMESH_SCENARIO_REVIEW_REPLAY_DIR")
	if dir == "" {
		t.Skip("opt in with the directory containing retained state.json and workspace.tar")
	}
	readJSON := func(name string, value any) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, value); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
	}
	var manifest struct{ Scenario string }
	var oracle scenarioReviewOracle
	var observations struct{ Statuses, Requests map[string]int }
	readJSON("manifest.json", &manifest)
	readJSON("fixture-oracle.json", &oracle)
	readJSON("fixture-observations.json", &observations)
	if (manifest.Scenario != "audit" && manifest.Scenario != "pentest") || len(oracle.Source) == 0 || oracle.Nonce == "" || len(oracle.Bodies) != 4 {
		t.Fatal("missing retained scenario or synthetic fixture oracle")
	}
	fixture := &scenarioSkillFixture{seen: observations.Statuses, reads: observations.Requests, source: oracle.Source, nonce: oracle.Nonce, bodies: oracle.Bodies}
	state, files, stateSHA256, workspaceSHA256 := readRetainedLiveEvidence(t, dir)
	t.Logf("validation_scope=scenario_skills_review_and_parallel workspace_sha256=%s state_sha256=%s", workspaceSHA256, stateSHA256)
	failures := validateScenarioSkillSteps(state, files, manifest.Scenario, scenarioSkillBundle(t, manifest.Scenario), fixture, 2)
	failures = append(failures, validateScenarioReviewCuration(state)...)
	parallel, parallelFailures := validateLiveParallelWorkers(state, files, 2)
	failures = append(failures, parallelFailures...)
	t.Logf("independent_workers=%d model_overlap_seconds=%.6f", len(parallel.Workers), parallel.OverlapSeconds)
	if len(failures) != 0 {
		t.Fatal(failures)
	}
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
