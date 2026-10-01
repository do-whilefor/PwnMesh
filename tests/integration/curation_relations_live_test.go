//go:build linux

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/dispatcher"
	"pwnmesh/internal/docker"
	"pwnmesh/internal/server"
	"pwnmesh/internal/worker"
)

const curationValuesPath = "/workspace/curation-values.txt"
const curationOriginalPath = "/workspace/curation-transcription.txt"
const curationCorrectedPath = "/workspace/curation-calculation.json"
const curationValues = "2\n3\n5\n"
const curationOriginal = "sum=11\n"
const curationOldClaim = "The sum of sample-v1 values is 11 (initial unverified transcription)"
const curationNewClaim = "The sum of sample-v1 values is 10 (recalculated from the retained input)"
const curationReason = "Recalculation corrects the initial transcription of the same immutable dataset; both observations and their evidence remain retained."

// This workload corrects an interpretation of immutable input. It does not
// invalidate a truthful observation merely because a mutable target changed.
func TestLiveCurationRelationsProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_CURATION_RELATIONS_TEST") != "1" {
		t.Skip("opt in to the real model and Docker curation-relations acceptance")
	}
	origin := fmt.Sprintf(`Exercise observation correction using ONLY synthetic local files under /workspace. Do not read credentials, install packages, or contact other services.
Authorize exactly one ordinary Step. Its Worker must first write these exact bytes to %s: %q, and these exact initial unverified transcription bytes to %s: %q. Publish the mistaken interpretation as a fact with exactly this description: %q, scope sample-v1, and evidence from the transcription file. This is an intentionally wrong interpretation of the dataset, not a claim that a historical event never happened.
Next, in the SAME Step, use a deterministic bash command to read every integer from the retained values file, calculate the sum, and compute SHA-256 of its exact bytes. Save AND print a JSON receipt to %s with fields sum (computed, never hardcoded) and dataset_sha256. Publish a second fact with exactly this description: %q, scope sample-v1, and evidence from BOTH the unchanged values file and the calculation receipt. Finish the Step with the second fact as its result. Preserve all three files. No candidate, dispute, report, or additional Step is needed.
The blackboard Agent must submit one curate action with groups:[] and relations:[{kind:"supersedes",source:the second fact ID,target:the first fact ID,reason:the calculation corrected the initial interpretation}]. Never rewrite or delete the original fact or evidence. The main Agent may complete only after this relation has been curated, citing the effective recalculated fact.`, curationValuesPath, curationValues, curationOriginalPath, curationOriginal, curationOldClaim, curationCorrectedPath, curationNewClaim)
	runObservedProject(t, "Correct an observation while retaining original evidence", origin, "Correct the initial sum=11 interpretation with a reproducible sum=10 calculation, atomically curate the supersedes relation, retain all original evidence, and complete using only effective support.", "atomic_curation_relation_with_retained_evidence", validateCurationRelationsDelivery, 1)
}

func validateCurationRelationsDelivery(state board.State, files map[string][]byte) []string {
	failures := []string{}
	check := func(ok bool, message string) {
		if !ok {
			failures = append(failures, message)
		}
	}
	check(state.Graph.Project.Status == "completed" && state.Graph.Project.OrchestrationVersion == 1, "curation-relations project did not complete in orchestration mode")
	check(bytes.Equal(files[curationValuesPath], []byte(curationValues)), "original immutable input bytes changed or disappeared")
	check(bytes.Equal(files[curationOriginalPath], []byte(curationOriginal)), "original incorrect transcription bytes changed or disappeared")
	var receipt struct {
		Sum    int    `json:"sum"`
		SHA256 string `json:"dataset_sha256"`
	}
	check(json.Unmarshal(files[curationCorrectedPath], &receipt) == nil && receipt.Sum == 2+3+5 && receipt.SHA256 == fmt.Sprintf("%x", sha256.Sum256([]byte(curationValues))), "calculation receipt does not match the retained input and independently checked sum")
	facts := map[string]board.FactRecord{}
	var old, corrected board.FactRecord
	for _, fact := range state.FactRecords {
		facts[fact.ID] = fact
		if fact.Description == curationOldClaim {
			old = fact
		}
		if fact.Description == curationNewClaim {
			corrected = fact
		}
	}
	check(old.ID != "" && corrected.ID != "" && old.ID != corrected.ID, "original and corrected facts were not both retained")
	if old.ID == "" || corrected.ID == "" {
		return failures
	}
	check(old.Scope == "sample-v1" && corrected.Scope == old.Scope && !old.Legacy && !corrected.Legacy, "observation scope or provenance changed")
	check(old.Status == "superseded" && corrected.Status == "valid" && !corrected.SupportInvalid, "correction did not invalidate only the initial interpretation")
	check(state.ValidateFactSources([]string{old.ID}, true) != nil && state.ValidateFactSources([]string{corrected.ID}, true) == nil, "obsolete support remains usable or corrected support is unusable")
	check(old.RunID != "" && old.RunID == corrected.RunID && old.SourceStepID != "" && old.SourceStepID == corrected.SourceStepID, "observations lost their single producer Step/run provenance")
	hasEvidence := func(fact board.FactRecord, raw []byte) bool {
		found := false
		for _, ref := range fact.Evidence {
			check(orchestrationEvidenceValid(ref, fact.RunID, files), "evidence bytes, hash or source run changed: "+fact.ID)
			found = found || bytes.Equal(files[ref.Path], raw)
		}
		return found
	}
	check(hasEvidence(old, []byte(curationOriginal)), "original fact lost its original transcription evidence")
	check(hasEvidence(corrected, []byte(curationValues)) && hasEvidence(corrected, files[curationCorrectedPath]), "corrected fact does not retain both actual input and calculation receipt")
	check(orchestrationCommandOutput(corrected.RunID, files[curationCorrectedPath], files), "calculation receipt was not printed by a successful Worker bash command")
	var steps []board.Step
	for _, step := range state.Steps {
		if !staleRepairCompletionMarker(state, step) {
			steps = append(steps, step)
		}
	}
	check(len(steps) == 1, "expected exactly one producer Step")
	if len(steps) == 1 {
		step := steps[0]
		_, ok := orchestrationSuccessfulRun(state.Graph.Project, step, corrected.RunID, facts, files)
		check(ok && step.ID == corrected.SourceStepID && step.Result != nil && *step.Result == corrected.ID, "producer did not successfully finish with the effective corrected fact")
	}
	check(len(state.FactRelations) == 1, "expected exactly one retained supersedes relation")
	if len(state.FactRelations) == 1 {
		relation := state.FactRelations[0]
		check(relation.Kind == "supersedes" && relation.Source == corrected.ID && relation.Target == old.ID && relation.Reason != "" && relation.CreatedAt != "", "relation lost its direction, explanation or creation time")
		var job worker.Job
		run := orchestrationRunID(relation.RunID)
		decoded := json.Unmarshal(files["/workspace/.pwnmesh/runs/"+run+"/job.json"], &job) == nil
		input := retainedCurationInput(job, files)
		check(run != "" && run != orchestrationRunID(corrected.RunID) && decoded && job.Kind == "curate" && input != nil && input.Revision <= state.Curation.ThroughRevision, "relation was not produced by a curator with a covered immutable input")
		if input != nil {
			check(input.ValidateFactSources([]string{old.ID, corrected.ID}, true) == nil, "curator did not observe both original facts before correction")
			for _, original := range input.FactRecords {
				if original.ID != old.ID && original.ID != corrected.ID {
					continue
				}
				retained := facts[original.ID]
				// Status is a derived projection of relations; the immutable
				// observation, timestamp, producer and evidence must not change.
				retained.Status, retained.SupportInvalid = original.Status, original.SupportInvalid
				check(reflect.DeepEqual(retained, original), "curation rewrote an original observation: "+original.ID)
			}
		}
	}
	check(state.Curation.Generation == state.Graph.Project.Generation && state.Curation.ThroughRevision > 0 && state.Curation.ThroughRevision < state.Revision, "curation cursor is missing or outside the committed state")
	rootSupported := false
	for _, goal := range state.Goals {
		if goal.ID == "goal" {
			rootSupported = goal.Status == "achieved" && goal.SupportValid && slices.Contains(goal.Sources, corrected.ID) && !slices.Contains(goal.Sources, old.ID)
		}
	}
	check(rootSupported, "project completion does not cite the effective corrected fact")
	check(len(state.Candidates) == 0 && len(state.Disputes) == 0, "unexpected candidate/dispute work in the simple observation-correction workload")
	return failures
}

// The local model controls protocol choices; Docker executes the production
// Go Loop, real file tools, graph bridge, curation transaction and completion.
func TestDockerCurationRelations(t *testing.T) {
	runDockerCurationRelations(t, false)
}

func runDockerCurationRelations(t *testing.T, withCandidates bool) {
	t.Helper()
	image := os.Getenv("PWNMESH_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set PWNMESH_DOCKER_TEST_IMAGE for Docker curation-relations acceptance")
	}
	started := time.Now()
	store, err := board.Open(filepath.Join(t.TempDir(), "curation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := httptest.NewServer(server.New(store))
	defer api.Close()
	client := &dispatcher.Client{Base: api.URL}
	var project board.Graph
	if err := client.Do(context.Background(), "POST", "/projects", map[string]any{"title": "Controlled observation correction", "origin": "Use only synthetic local files and retain the original observation.", "goal": "Correct the mistaken sum and complete using effective evidence.", "bootstrap_enabled": false, "orchestration_version": 1}, &project, nil); err != nil {
		t.Fatal(err)
	}
	modelErrors := make(chan error, 16)
	var mu sync.Mutex
	turns := map[string]int{}
	var oldID, correctedID, oldCandidateID, freshCandidateID string
	rollbackChecked := false
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fail := func(err error) {
			select {
			case modelErrors <- err:
			default:
			}
			http.Error(w, "scripted curation assertion failed", http.StatusBadRequest)
		}
		var request struct {
			Messages []agent.Message `json:"messages"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&request); err != nil {
			fail(err)
			return
		}
		run := r.Header.Get("x-opencode-session")
		var execution board.Execution
		if err := store.Do(r.Context(), func(tx *board.Tx) error {
			var err error
			execution, err = tx.Execution(project.Project.ID, run)
			return err
		}); err != nil {
			fail(err)
			return
		}
		var job worker.Job
		if err := json.Unmarshal(execution.Job, &job); err != nil {
			fail(err)
			return
		}
		turns[run]++
		turn := turns[run]
		respond := func(block agent.Block, stop string) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"role": "assistant", "content": []agent.Block{block}, "stop_reason": stop})
		}
		call := func(name string, input any) {
			raw, _ := json.Marshal(input)
			respond(agent.Block{Type: "tool_use", ID: fmt.Sprintf("call-%d", turn), Name: name, Input: raw}, "tool_use")
		}
		action := func(op, key string, payload any) {
			call("graph_action", map[string]any{"op": op, "idempotency_key": key, "payload": payload})
		}
		last := func(wantError bool) (string, error) {
			id := fmt.Sprintf("call-%d", turn-1)
			for n := len(request.Messages) - 1; n >= 0; n-- {
				for _, b := range request.Messages[n].Content {
					if b.Type == "tool_result" && b.ToolUseID == id {
						if b.IsError != wantError {
							return "", fmt.Errorf("%s %s unexpected error=%t: %s", job.Kind, id, b.IsError, b.Content)
						}
						var text string
						err := json.Unmarshal(b.Content, &text)
						return text, err
					}
				}
			}
			return "", fmt.Errorf("missing %s result %s", job.Kind, id)
		}
		if turn > 1 {
			if _, err := last(job.Kind == "curate" && turn == 2); err != nil {
				fail(err)
				return
			}
		}
		switch job.Kind {
		case "reason":
			switch turn {
			case 1:
				call("read_graph", map[string]any{"section": "steps", "limit": 20})
			case 2:
				text, _ := last(false)
				var page struct {
					Items []board.Step `json:"items"`
				}
				if err := json.Unmarshal([]byte(text), &page); err != nil {
					fail(err)
					return
				}
				if len(page.Items) == 0 {
					action("step", "producer", map[string]any{"action": "add", "from": []string{"origin"}, "description": "Retain the mistaken transcription, recalculate the same fixed input and publish a correction."})
				} else if len(page.Items) == 1 && page.Items[0].Status == "completed" && correctedID != "" {
					var state board.State
					if err := store.Do(r.Context(), func(tx *board.Tx) error {
						var err error
						state, err = tx.State(project.Project.ID)
						return err
					}); err != nil {
						fail(err)
						return
					}
					if state.Curation.ThroughRevision == 0 {
						// This workload requires reconciliation even though its facts
						// could otherwise support goal-scoped completion directly.
						action("curation_request", "review-correction", map[string]any{"sources": []string{oldID, correctedID}, "reason": curationReason})
						return
					}
					action("complete", "finish", map[string]any{"from": []string{correctedID}, "description": "Recalculation corrected the mistaken interpretation; original evidence and curator relation are retained."})
				} else {
					fail(fmt.Errorf("unexpected planner Steps: %+v", page.Items))
				}
			case 3:
				text, _ := last(false)
				var draft struct {
					Op    string `json:"op"`
					Draft bool   `json:"draft"`
				}
				if json.Unmarshal([]byte(text), &draft) != nil || !draft.Draft {
					fail(errors.New("missing private planner draft"))
					return
				}
				if draft.Op == "complete" {
					action("preview", "preview", map[string]any{})
				} else {
					action("commit", "commit", map[string]any{})
				}
			case 4:
				text, _ := last(false)
				var preview board.DecisionReceipt
				if json.Unmarshal([]byte(text), &preview) != nil || preview.CompletionReview == nil {
					fail(errors.New("missing completion review"))
					return
				}
				action("commit", "commit", map[string]any{})
			default:
				fail(errors.New("planner continued after commit"))
			}
		case "explore":
			captureID := func(label string, target *string) bool {
				text, _ := last(false)
				var receipt board.StateActionResult
				if json.Unmarshal([]byte(text), &receipt) != nil || receipt.ID == "" {
					fail(fmt.Errorf("missing %s receipt", label))
					return false
				}
				*target = receipt.ID
				return true
			}
			calculate := func() {
				command := `set -eu; sum=0; while IFS= read -r value; do sum=$((sum + value)); done < ` + curationValuesPath + `; hash=$(sha256sum ` + curationValuesPath + `); hash=${hash%% *}; printf '{"sum":%d,"dataset_sha256":"%s"}\n' "$sum" "$hash" | tee ` + curationCorrectedPath
				call("bash", map[string]any{"command": command, "timeout": 10})
			}
			correct := func() {
				action("fact", "correction", map[string]any{"description": curationNewClaim, "scope": "sample-v1", "observed_at": time.Now().UTC().Format(time.RFC3339), "evidence": []map[string]any{{"path": curationValuesPath, "start_line": 1, "end_line": 3}, {"path": curationCorrectedPath, "start_line": 1, "end_line": 1}}})
			}
			finish := func() {
				result, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact_id": correctedID}})
				respond(agent.Block{Type: "text", Text: string(result)}, "end_turn")
			}
			switch turn {
			case 1:
				call("write", map[string]any{"path": curationValuesPath, "content": curationValues})
			case 2:
				call("write", map[string]any{"path": curationOriginalPath, "content": curationOriginal})
			case 3:
				action("fact", "original", map[string]any{"description": curationOldClaim, "scope": "sample-v1", "observed_at": time.Now().UTC().Format(time.RFC3339), "evidence": []map[string]any{{"path": curationOriginalPath, "start_line": 1, "end_line": 1}}})
			case 4:
				if !captureID("original fact", &oldID) {
					return
				}
				if withCandidates {
					action("candidate", "tentative", map[string]any{"claim": curationSupportClaim, "scope": "sample-v1", "status": "candidate", "sources": []string{oldID}, "evidence": []map[string]any{{"path": curationOriginalPath, "start_line": 1, "end_line": 1}}, "reason": "The initial transcription is unverified; the common claim remains tentative."})
				} else {
					calculate()
				}
			case 5:
				if withCandidates {
					if !captureID("tentative candidate", &oldCandidateID) {
						return
					}
					calculate()
				} else {
					correct()
				}
			case 6:
				if withCandidates {
					correct()
				} else if captureID("corrected fact", &correctedID) {
					finish()
				}
			case 7:
				if !withCandidates || !captureID("corrected fact", &correctedID) {
					return
				}
				action("candidate", "verified", map[string]any{"claim": curationSupportClaim, "scope": "sample-v1", "status": "verified", "sources": []string{correctedID}, "evidence": []map[string]any{{"path": curationValuesPath, "start_line": 1, "end_line": 3}, {"path": curationCorrectedPath, "start_line": 1, "end_line": 1}}, "reason": "The retained input was independently summed by the deterministic command."})
			case 8:
				if withCandidates && captureID("verified candidate", &freshCandidateID) {
					finish()
				}
			default:
				fail(errors.New("unexpected producer turn"))
			}
		case "curate":
			if oldID == "" || correctedID == "" || job.InputSnapshot == nil {
				fail(errors.New("curator ran before both observations were available"))
				return
			}
			relation := map[string]any{"kind": "supersedes", "source": correctedID, "target": oldID, "reason": curationReason}
			groups := []board.CurateGroup{}
			if withCandidates {
				if oldCandidateID == "" || freshCandidateID == "" {
					fail(errors.New("curator ran before both candidate judgments were available"))
					return
				}
				groups = append(groups, board.CurateGroup{CandidateIDs: []string{oldCandidateID, freshCandidateID}, Status: "verified", Reason: "The fresh calculation supports the claim; retain the tentative candidate but exclude its superseded premise."})
			}
			switch turn {
			case 1:
				// The first relation would be valid in isolation. The protected
				// origin target must reject the entire batch, including its cursor.
				action("curate", "invalid-batch", map[string]any{"groups": groups, "relations": []any{relation, map[string]any{"kind": "refutes", "source": correctedID, "target": "origin", "reason": "must be rejected"}}})
			case 2:
				var current board.State
				var frozen board.State
				if err := store.Do(r.Context(), func(tx *board.Tx) error {
					var err error
					current, err = tx.State(project.Project.ID)
					if err == nil {
						frozen, err = tx.ReadInputSnapshot(project.Project.ID, job.InputSnapshot.ID)
					}
					return err
				}); err != nil {
					fail(err)
					return
				}
				if len(current.FactRelations) != 0 || !reflect.DeepEqual(current.Curation, frozen.Curation) || current.Revision != frozen.Revision {
					fail(errors.New("rejected curation changed relations, revision or cursor"))
					return
				}
				rollbackChecked = true
				action("curate", "valid-batch", map[string]any{"groups": groups, "relations": []any{relation}})
			default:
				fail(errors.New("curator continued after its committed receipt"))
			}
		default:
			fail(fmt.Errorf("unexpected role %s", job.Kind))
		}
	}))
	defer model.Close()
	conf := config.Config{Server: api.URL, Runtime: config.Runtime{Interval: 1, MaxWorkers: 1, MaxProjects: 1, MaxProjectWorkers: 1, HealthMode: "disabled", HealthTimeout: 10}, Tasks: config.Tasks{Reason: config.Task{Timeout: 30, MaxIntents: 1}, Curate: config.Task{Timeout: 30}, Explore: config.Task{Timeout: 30, ConcludeTimeout: 5}}, Container: config.Container{Image: image, Network: testContainerNetwork(t), Namespace: fmt.Sprintf("pwnmesh-curation-%d", time.Now().UnixNano()), CompletedAction: "stop"}, Workers: []config.Worker{{Name: "controlled", Type: "go", TaskTypes: []string{"reason", "curate", "explore"}, MaxRunning: 1, Env: map[string]string{"ANTHROPIC_BASE_URL": model.URL, "ANTHROPIC_AUTH_TOKEN": "controlled-test-only", "ANTHROPIC_MODEL": "controlled", "PWNMESH_REQUEST_TIMEOUT": "10"}}}}
	if err := conf.Validate(); err != nil {
		t.Fatal(err)
	}
	runner := &liveObservedRunner{Client: docker.New(conf.Container)}
	defer runner.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := runner.Cleanup(ctx, project.Project.ID, "deleted"); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	done := make(chan error, 1)
	go func() { done <- dispatcher.New(conf, runner).Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("curation dispatcher shutdown timed out")
		}
	}()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var state board.State
	for state.Graph.Project.Status != "completed" {
		select {
		case err := <-modelErrors:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("curation project did not complete", ctx.Err())
		case <-tick.C:
		}
		if err := client.Do(ctx, "GET", "/projects/"+project.Project.ID+"/state", nil, &state, nil); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("phase=project_completed elapsed=%.3fs model=local-scripted", time.Since(started).Seconds())
	// Complete is visible before its Worker has flushed the final session.
	// Let that receipt settle before reading evidence or reporting run times.
	settled, settleCancel := context.WithTimeout(ctx, 5*time.Second)
	defer settleCancel()
	for {
		active := false
		for _, run := range runner.snapshot() {
			active = active || run.Finished.IsZero()
		}
		if !active {
			break
		}
		select {
		case <-tick.C:
		case <-settled.Done():
			t.Fatal("completed project still has an unfinished Worker receipt")
		}
	}
	files, err := collectLiveWorkspace(ctx, conf.Container.Namespace+"-dispatch-"+project.Project.ID, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := retainCurationSnapshots(ctx, store, project.Project.ID, files, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	validate := validateCurationRelationsDelivery
	if withCandidates {
		validate = validateCurationSupportDelivery
	}
	if failures := validate(state, files); len(failures) > 0 {
		t.Fatal(failures)
	}
	mu.Lock()
	checked := rollbackChecked
	mu.Unlock()
	if !checked {
		t.Fatal("invalid atomic curation batch was not exercised")
	}
	var events []board.StateEvent
	if err := client.Do(ctx, "GET", "/projects/"+project.Project.ID+"/state/events?after=0", nil, &events, nil); err != nil {
		t.Fatal(err)
	}
	curations, covered := 0, int64(0)
	for _, event := range events {
		switch event.Op {
		case "fact", "candidate", "step_completed":
			if event.Revision > covered {
				covered = event.Revision
			}
		case "curate":
			curations++
			var input struct {
				ThroughRevision int64                `json:"through_revision"`
				Relations       []board.FactRelation `json:"relations"`
			}
			if json.Unmarshal(event.Payload, &input) != nil || len(input.Relations) != 1 || input.ThroughRevision < covered || input.ThroughRevision >= event.Revision {
				t.Fatal("committed curation did not atomically cover prior observations and its relation")
			}
		}
	}
	if curations != 1 || state.Curation.ThroughRevision < covered {
		t.Fatal("invalid batch persisted or curation cursor skipped required observation coverage")
	}
	for _, run := range runner.snapshot() {
		t.Logf("phase=run kind=%s elapsed=%.3fs status=%s model=local-scripted", run.Kind, run.Finished.Sub(run.Started).Seconds(), run.Status)
	}
	select {
	case err := <-modelErrors:
		t.Fatal(err)
	default:
	}
}

func curationRelationsDeliveryFixture(t *testing.T) (board.State, map[string][]byte) {
	t.Helper()
	const observed = "2026-09-28T00:00:00Z"
	workerID, factID := "controlled@producer-run", "corrected"
	state := board.State{Graph: board.Graph{Project: board.Project{ID: "curation-project", Status: "completed", OrchestrationVersion: 1}}, Revision: 5,
		Curation: board.CurationProgress{ThroughRevision: 3, RunID: "controlled@curator-run", UpdatedAt: observed}}
	files := map[string][]byte{curationValuesPath: []byte(curationValues), curationOriginalPath: []byte(curationOriginal),
		curationCorrectedPath: []byte(fmt.Sprintf("{\"sum\":10,\"dataset_sha256\":\"%x\"}\n", sha256.Sum256([]byte(curationValues))))}
	old := board.FactRecord{ID: "original", Description: curationOldClaim, Scope: "sample-v1", ObservedAt: observed, Status: "superseded", RunID: workerID, SourceStepID: "producer",
		Evidence: []board.EvidenceRef{retainOrchestrationEvidence("producer-run", []byte(curationOriginal), files)}}
	corrected := board.FactRecord{ID: factID, Description: curationNewClaim, Scope: "sample-v1", ObservedAt: observed, Status: "valid", RunID: workerID, SourceStepID: "producer",
		Evidence: []board.EvidenceRef{retainOrchestrationEvidence("producer-run", []byte(curationValues), files), retainOrchestrationEvidence("producer-run", files[curationCorrectedPath], files)}}
	state.FactRecords = []board.FactRecord{old, corrected}
	state.Steps = []board.Step{{ID: "producer", Status: "completed", SupportValid: true, Worker: &workerID, Result: &factID}}
	state.Goals = []board.Goal{{ID: "goal", Status: "achieved", SupportValid: true, Sources: []string{factID}}}
	state.FactRelations = []board.FactRelation{{Kind: "supersedes", Source: factID, Target: old.ID, Reason: curationReason, RunID: state.Curation.RunID, CreatedAt: observed}}
	base := "/workspace/.pwnmesh/runs/producer-run/"
	files[base+"job.json"], _ = json.Marshal(map[string]any{"run_id": "producer-run", "kind": "explore", "graph_rpc": true, "result_contract_version": 2, "graph": state.Graph, "intent": map[string]string{"id": "producer"}})
	files[base+"session.json"], _ = json.Marshal(map[string]any{"run_id": "producer-run", "identity": map[string]string{"project_id": state.Graph.Project.ID, "step_id": "producer", "run_id": "producer-run"},
		"result": map[string]any{"status": "success", "text": `{"accepted":true,"outcome":"completed","data":{"fact_id":"corrected"}}`}})
	output, _ := json.Marshal(string(files[curationCorrectedPath]))
	for _, message := range []agent.Message{
		{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "calculate", Name: "bash", Input: json.RawMessage(`{"command":"sum actual input"}`)}}},
		{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "calculate", Content: output}}},
	} {
		raw, _ := json.Marshal(agent.Event{Type: "message", Message: &message})
		files[base+"events.jsonl"] = append(files[base+"events.jsonl"], append(raw, '\n')...)
	}
	old.Status = "valid"
	files["/workspace/.pwnmesh/runs/curator-run/job.json"], _ = json.Marshal(worker.Job{Kind: "curate", State: &board.State{Revision: 3, FactRecords: []board.FactRecord{old, corrected}}})
	return state, files
}

func TestCurationRelationsDeliveryValidatesRetainedProof(t *testing.T) {
	state, files := curationRelationsDeliveryFixture(t)
	if failures := validateCurationRelationsDelivery(state, files); len(failures) != 0 {
		t.Fatal(failures)
	}
	for _, test := range []struct {
		name   string
		change func(*board.State, map[string][]byte)
	}{
		{"completion_without_facts", func(s *board.State, _ map[string][]byte) { s.FactRecords = nil }},
		{"original_bytes_changed", func(_ *board.State, f map[string][]byte) { f[curationOriginalPath] = []byte("sum=10\n") }},
		{"wrong_calculation", func(_ *board.State, f map[string][]byte) { f[curationCorrectedPath] = []byte(`{"sum":11}`) }},
		{"lost_original_evidence", func(s *board.State, _ map[string][]byte) { s.FactRecords[0].Evidence = nil }},
		{"lost_input_evidence", func(s *board.State, _ map[string][]byte) { s.FactRecords[1].Evidence = s.FactRecords[1].Evidence[1:] }},
		{"original_still_effective", func(s *board.State, _ map[string][]byte) { s.FactRecords[0].Status = "valid" }},
		{"reversed_relation", func(s *board.State, _ map[string][]byte) {
			s.FactRelations[0].Source, s.FactRelations[0].Target = "original", "corrected"
		}},
		{"wrong_relation_producer", func(s *board.State, _ map[string][]byte) { s.FactRelations[0].RunID = "controlled@producer-run" }},
		{"original_observation_rewritten", func(s *board.State, _ map[string][]byte) { s.FactRecords[0].ObservedAt = "2026-09-28T01:00:00Z" }},
		{"cursor_not_advanced", func(s *board.State, _ map[string][]byte) { s.Curation.ThroughRevision = 0 }},
		{"completion_uses_obsolete_fact", func(s *board.State, _ map[string][]byte) { s.Goals[0].Sources = []string{"original"} }},
		{"no_successful_worker_result", func(_ *board.State, f map[string][]byte) {
			delete(f, "/workspace/.pwnmesh/runs/producer-run/session.json")
		}},
		{"no_actual_command", func(_ *board.State, f map[string][]byte) {
			delete(f, "/workspace/.pwnmesh/runs/producer-run/events.jsonl")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, files := curationRelationsDeliveryFixture(t)
			test.change(&state, files)
			if failures := validateCurationRelationsDelivery(state, files); len(failures) == 0 {
				t.Fatal("broken observation-correction proof was accepted")
			}
		})
	}
}
