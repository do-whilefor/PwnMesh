package dispatcher

import (
	"context"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/worker"
)

func TestUrgentReasonConflictRestoresUnconsumedDependencyCorrection(t *testing.T) {
	fixture, _, _, graph := automaticRetryFixture(t, 0, "")
	runner := &conflictDecisionRunner{batchProtocolRunner: batchProtocolRunner{directions: 1}, client: fixture.Client, conflictOp: "decision_commit"}
	s := New(fixture.Config, runner)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer func() { cancel(); s.wg.Wait() }()
	id := graph.Project.ID
	// Exercise the scheduling metadata boundary separately from the real HTTP
	// registration/commit protocol. The same invalid Step remains on later pages;
	// no second transition can restore urgency after launch clears reasonWaits.
	g := board.Graph{Project: graph.Project, Intents: []board.Intent{{ID: "working", Worker: board.Ptr("other@execute")}}}
	previous := board.SchedulePage{FactCount: 2, OpenCount: 1, DecisionRevision: 1}
	input := board.SchedulePage{FactCount: 2, OpenCount: 1, DecisionRevision: 2, Steps: []board.Step{{ID: "queued", InvalidSources: []string{"refuted"}}}}
	s.checkpoints[id] = checkpoint{Facts: 2, Open: 1}
	s.decisionRevisions[id], s.stateRevisions[id] = 1, 2
	s.schedules[id] = input
	trigger := s.trigger(g, board.ExecutionCheck{}, previous, time.Now())
	if trigger == "" || !s.reasonWaits[id].Urgent {
		t.Fatal("new invalid dependency did not request urgent planning")
	}
	if ok, err := s.launch(ctx, graph, "reason", nil, trigger, board.ExecutionCheck{}); !ok || err != nil {
		t.Fatalf("urgent planner did not launch: %v %v", ok, err)
	}
	if s.reasonWaits[id].Urgent {
		t.Fatal("launch did not consume its pending scheduling signal")
	}
	s.wg.Wait() // The runner publishes a concurrent update, then receives real 409.
	s.reap()
	if !s.reasonWaits[id].Urgent || s.controlConflicts[id].IsZero() {
		t.Fatal("confirmed stale failure lost its unconsumed invalid dependency")
	}
	if got := s.trigger(g, board.ExecutionCheck{}, input, time.Now()); got == "" {
		t.Fatal("unchanged invalid dependency page delayed replacement for sixty seconds")
	}
	if err := s.Client.Do(ctx, "GET", projectPath(id), nil, &graph, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.launch(ctx, graph, "reason", nil, trigger, board.ExecutionCheck{}); !ok || err != nil {
		t.Fatalf("fresh urgent replacement did not launch: %v %v", ok, err)
	}
	s.wg.Wait()
	s.reap()
	if s.reasonWaits[id].Urgent || len(runner.seen) != 2 {
		t.Fatal("successful replacement left permanent urgency or repeated planning")
	}
	// A later ordinary update uses normal coalescing even though the invalid
	// historical Step still appears in both pages.
	s.checkpoints[id] = checkpoint{Facts: input.FactCount, Open: input.OpenCount}
	s.decisionRevisions[id] = input.DecisionRevision
	previous = input
	input.FactCount++
	input.DecisionRevision++
	s.schedules[id], s.stateRevisions[id] = input, input.DecisionRevision
	if got := s.trigger(g, board.ExecutionCheck{}, previous, time.Now()); got != "" {
		t.Fatalf("consumed dependency correction disabled ordinary coalescing: %q", got)
	}
}

func TestFailedReasonUrgencyDoesNotCrossGenerationOrInventUrgency(t *testing.T) {
	for _, mode := range []string{"urgent_failure", "ordinary_failure", "new_generation", "success"} {
		t.Run(mode, func(t *testing.T) {
			s := New(config.Config{Runtime: config.Runtime{MaxWorkers: 1}}, nil)
			task := &task{Job: worker.Job{Kind: "reason", Graph: board.Graph{Project: board.Project{ID: "p"}}}, urgentInput: true}
			outcome := "failed"
			switch mode {
			case "ordinary_failure":
				task.urgentInput = false
			case "new_generation":
				s.generations["p"] = 1
			case "success":
				outcome = "success"
			}
			s.done <- finished{Task: task, Outcome: outcome}
			s.reap()
			if got := s.reasonWaits["p"].Urgent; got != (mode == "urgent_failure") {
				t.Fatalf("unexpected restored urgency: %v", got)
			}
		})
	}
}
