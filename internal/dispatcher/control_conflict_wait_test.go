package dispatcher

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/worker"
)

func TestControlConflictWaitDoesNotExtendOnObservationsOrPolling(t *testing.T) {
	for _, kind := range []string{"reason", "curate"} {
		t.Run(kind, func(t *testing.T) {
			s, g, previous, now := coalescingFixture()
			until := now.Add(reasonMaxWait)
			s.controlConflicts[g.Project.ID] = until
			for _, second := range []int{0, 16, 30, 59, 60, 61, 90} {
				input := s.schedules[g.Project.ID]
				input.FactCount++
				input.Revision++
				input.DecisionRevision++
				s.schedules[g.Project.ID] = input
				s.stateRevisions[g.Project.ID] = input.DecisionRevision
				s.nextWake = time.Time{}
				at := now.Add(time.Duration(second) * time.Second)
				waiting := false
				if kind == "reason" {
					waiting = s.trigger(g, board.ExecutionCheck{}, previous, at) == ""
				} else {
					waiting = s.waitForCuration(g, input.Revision, at)
				}
				if waiting != (second < 60) || !s.controlConflicts[g.Project.ID].Equal(until) {
					t.Fatalf("second %d: waiting=%v deadline=%s", second, waiting, s.controlConflicts[g.Project.ID])
				}
				if waiting && !s.nextWake.Equal(until) || !waiting && !s.nextWake.IsZero() {
					t.Fatalf("second %d: conflict wake moved or spun: %s", second, s.nextWake)
				}
				previous = input
			}
		})
	}
}

func TestControlConflictWaitPreservesUrgentInputsAndDrain(t *testing.T) {
	for _, kind := range []string{"reason", "curate"} {
		for _, change := range []string{"hint", "invalid_dependency", "blocked_dependency", "idle"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				s, g, previous, now := coalescingFixture()
				s.controlConflicts[g.Project.ID] = now.Add(reasonMaxWait)
				input := s.schedules[g.Project.ID]
				switch change {
				case "hint":
					input.HintCount++
				case "invalid_dependency":
					input.Steps = []board.Step{{ID: "queued", InvalidSources: []string{"refuted"}}}
				case "blocked_dependency":
					input.Steps = []board.Step{{ID: "queued", BlockedBy: []string{"failed"}}}
				case "idle":
					g.Intents[0].Worker = nil
				}
				s.schedules[g.Project.ID] = input
				// dispatch records invalidation before deciding either role's readiness.
				s.noteInvalidDependencies(g.Project.ID, previous, input)
				if kind == "reason" {
					if got := s.trigger(g, board.ExecutionCheck{}, previous, now); got == "" {
						t.Fatal("urgent or drained decision was delayed by stale input")
					}
				} else if s.waitForCuration(g, input.Revision, now) {
					t.Fatal("urgent or drained curation was delayed by stale input")
				}
				if change == "idle" && !s.controlConflicts[g.Project.ID].IsZero() {
					t.Fatal("drained producer wave retained conflict backoff")
				}
			})
		}
	}
	s, g, previous, now := coalescingFixture()
	s.controlConflicts[g.Project.ID] = now.Add(reasonMaxWait)
	if got := s.trigger(g, board.ExecutionCheck{PreviousRunID: "authorized"}, previous, now); got != "explicit_retry" {
		t.Fatalf("explicit retry was delayed: %q", got)
	}
}

func TestControlConflictWaitIsProjectAndGenerationScoped(t *testing.T) {
	s, g, _, now := coalescingFixture()
	s.controlConflicts["unrelated"] = now.Add(reasonMaxWait)
	if !s.waitForCuration(g, 1, now) || s.waitForCuration(g, 1, now.Add(reasonQuietPeriod)) {
		t.Fatal("another project's conflict changed the normal fifteen-second quiet period")
	}
	s.controlConflicts[g.Project.ID] = now.Add(reasonMaxWait)
	g.Project.Generation++
	s.observeGeneration(g.Project)
	if !s.controlConflicts[g.Project.ID].IsZero() || s.controlConflicts["unrelated"].IsZero() {
		t.Fatal("new generation retained its old conflict or erased another project's wait")
	}
}

func TestControlConflictDeadlineUsesConfirmedFailureTime(t *testing.T) {
	for _, mode := range []string{"stale", "unconfirmed", "committed", "old_generation"} {
		t.Run(mode, func(t *testing.T) {
			s := New(config.Config{Runtime: config.Runtime{MaxWorkers: 1}}, nil)
			failedAt := time.Now().Add(-20 * time.Second)
			task := &task{Job: worker.Job{RunID: "run", Kind: "curate", Graph: board.Graph{Project: board.Project{ID: "p"}}}, staleInputAt: failedAt}
			outcome := "failed"
			switch mode {
			case "unconfirmed":
				task.staleInputAt = time.Time{}
			case "committed":
				outcome = "success"
			case "old_generation":
				s.generations["p"] = 1
			}
			s.done <- finished{Task: task, Outcome: outcome}
			s.reap()
			until, ok := s.controlConflicts["p"]
			if ok != (mode == "stale") || ok && !until.Equal(failedAt.Add(reasonMaxWait)) {
				t.Fatalf("reaping restarted or invented the conflict deadline: %s", until)
			}
		})
	}
	s := New(config.Config{Runtime: config.Runtime{MaxWorkers: 1}}, nil)
	latest := time.Now()
	for _, failure := range []time.Time{latest, latest.Add(-5 * time.Second)} {
		// Lease release may delay an older failed task's arrival in done.
		s.done <- finished{Task: &task{Job: worker.Job{Kind: "curate", Graph: board.Graph{Project: board.Project{ID: "p"}}}, staleInputAt: failure}, Outcome: "failed"}
		s.reap()
	}
	if !s.controlConflicts["p"].Equal(latest.Add(reasonMaxWait)) {
		t.Fatal("late delivery of an older failure replaced the newest deadline")
	}
}

func TestCurationUserCorrectionBypassesConflictWaitWithLiveProducers(t *testing.T) {
	s, r, _, graph, ctx := pipelineFixture(t, 3, false)
	r.reconcile = true
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		pipelineStarted(t, ctx, r)
	}
	s.controlConflicts[graph.Project.ID] = time.Now().Add(reasonMaxWait)
	if err := s.Client.Do(ctx, "POST", projectPath(graph.Project.ID)+"/hints", map[string]string{"content": "Reconsider the active plan now", "creator": "user"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
		t.Fatalf("new user input waited behind an active producer wave: %v %v", ok, err)
	}
	select {
	case <-r.curateStarted:
	case <-ctx.Done():
		t.Fatal("urgent curation was not launched")
	}
	if !s.controlConflicts[graph.Project.ID].IsZero() {
		t.Fatal("new control attempt retained the old failure's backoff")
	}
}

func TestConfirmedCuratorConflictDefersOnlyWhileProducersRun(t *testing.T) {
	s, r, store, graph, _ := pipelineFixture(t, 3, false)
	// This full conflict/replacement sequence outlasts the fixture's setup
	// budget under race-enabled package contention. A test deadline is a
	// dispatcher shutdown, which intentionally leaves a recoverable running row.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	r.reconcile = true
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	left, right := pipelineStarted(t, ctx, r), pipelineStarted(t, ctx, r)
	if left.Intent.Description != "left" {
		left, right = right, left
	}
	// The first attempt retains the existing fifteen-second quiet boundary.
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	s.curationWaits[graph.Project.ID] = reasonWait{First: time.Now().Add(-reasonMaxWait)}
	if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
		t.Fatalf("first curator did not start: %v %v", ok, err)
	}
	var curator worker.Job
	select {
	case curator = <-r.curateStarted:
	case <-ctx.Done():
		t.Fatal("curator did not enter its model request")
	}
	payload, _ := json.Marshal(map[string]any{"description": "A later observation", "scope": "left", "observed_at": time.Now().UTC().Format(time.RFC3339),
		"evidence": []board.EvidenceRef{{RunID: left.RunID, Path: "/workspace/later.txt", Excerpt: "later"}}})
	if _, err := r.graph(ctx, left, worker.GraphRequest{Op: "graph_action", Action: board.StateAction{Op: "fact", IdempotencyKey: "later", Payload: payload}}); err != nil {
		t.Fatal(err)
	}
	select {
	case done := <-s.done:
		if done.Task.Job.RunID != curator.RunID || done.Outcome != "failed" || done.Task.staleInputAt.IsZero() {
			t.Fatalf("stale curator did not record a confirmed failure: %+v", done)
		}
		s.done <- done
	case <-ctx.Done():
		t.Fatal("stale curator did not stop")
	}
	s.reap()
	until := s.controlConflicts[graph.Project.ID]
	if until.IsZero() {
		t.Fatal("confirmed state_changed did not establish bounded backoff")
	}
	for range 3 {
		if ok, err := s.dispatch(ctx, graph.Project.ID); ok || err != nil {
			t.Fatalf("same producer wave started another control attempt: %v %v", ok, err)
		}
		if !s.controlConflicts[graph.Project.ID].Equal(until) {
			t.Fatal("polling extended the deadline")
		}
	}
	close(r.release["left"])
	close(r.release["right"])
	s.wg.Wait()
	s.reap()
	if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
		t.Fatalf("backoff held already-authorized dependency work: %v %v", ok, err)
	}
	join := pipelineStarted(t, ctx, r)
	if join.Intent.Description != "join" || len(join.DependencyResults) != 2 || right.RunID == left.RunID {
		t.Fatal("accepted producer bindings were not retained")
	}
	close(r.release["join"])
	s.wg.Wait()
	s.reap()
	if ok, err := s.dispatch(ctx, graph.Project.ID); !ok || err != nil {
		t.Fatalf("drained wave did not immediately replace stale curation: %v %v", ok, err)
	}
	select {
	case replacement := <-r.curateStarted:
		if replacement.RunID == curator.RunID || replacement.InputSnapshot.StateVersion == curator.InputSnapshot.StateVersion {
			t.Fatal("replacement reused stale attempt or input")
		}
	case <-ctx.Done():
		t.Fatal("drained wave waited out backoff")
	}
	close(r.allowCurator)
	s.wg.Wait()
	if err := ctx.Err(); err != nil {
		t.Fatalf("curation scenario exceeded its test deadline: %v", err)
	}
	curations := 0
	for _, run := range testExecutions(t, store) {
		if run.Kind == "curate" {
			curations++
			if run.ID == curator.RunID && run.Status != "failed" || run.ID != curator.RunID && run.Status != "succeeded" {
				t.Fatalf("curation history was rewritten or fresh input failed: %s %s", run.ID, run.Status)
			}
		}
	}
	if curations != 2 {
		t.Fatalf("expected one stale and one fresh curator, got %d", curations)
	}
}
