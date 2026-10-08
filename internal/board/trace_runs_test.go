package board

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTraceRunsEnforcesProjectRoundAndExploreIdentity(t *testing.T) {
	s := executionQueryStore(t)
	executionQueryTx(t, s, func(tx *Tx) {
		put := func(project, id, kind string, generation int64) {
			job, _ := json.Marshal(map[string]any{"run_id": id, "kind": kind, "workspace": "/workspace/" + project, "graph": Graph{Project: Project{ID: project, Generation: generation}}, "intent": Intent{ID: "step"}})
			putQueryExecution(t, tx, Execution{ProjectID: project, ID: id, Kind: kind, Intent: "step", Job: job, Status: "running"}, generation, "")
		}
		put("p", "first", "explore", 0)
		put("p", "second", "explore", 0)
		put("p", "old-round", "explore", 1)
		put("p", "planner", "reason", 0)
		put("q", "foreign", "explore", 0)
		first, err := tx.TraceRuns("p", 0, 0, 1, "")
		if err != nil || len(first) != 1 || first[0].RunID != "first" || first[0].StepID != "step" || first[0].Workspace != "/workspace/p" || len(first[0].RegisteredJob) == 0 {
			t.Fatalf("first=%+v err=%v", first, err)
		}
		second, err := tx.TraceRuns("p", 0, 1, 50, "")
		if err != nil || len(second) != 1 || second[0].RunID != "second" {
			t.Fatalf("second=%+v err=%v", second, err)
		}
		for _, id := range []string{"old-round", "planner", "foreign", "missing"} {
			page, err := tx.TraceRuns("p", 0, 0, 1, id)
			if err != nil || len(page) != 0 {
				t.Fatalf("unregistered capability %s: %+v %v", id, page, err)
			}
		}
		if _, err := tx.TraceRuns("p", 1, 0, 1, ""); err == nil || !strings.Contains(err.Error(), "previous project round") {
			t.Fatalf("forged round accepted: %v", err)
		}
		if _, err := tx.Exec(`INSERT INTO xloom_project_rounds(project_id,generation,restarted_at) VALUES('p',2,?)`, tx.Now); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.TraceRuns("p", 0, 0, 1, "first"); err == nil {
			t.Fatal("previous round remained readable after restart")
		}
	})
}

func TestTraceRunsRejectsUnboundJobsAndBadBounds(t *testing.T) {
	s := executionQueryStore(t)
	executionQueryTx(t, s, func(tx *Tx) {
		putQueryExecution(t, tx, Execution{ID: "unbound", Kind: "explore", Intent: "step"}, 0, "")
		if _, err := tx.TraceRuns("p", 0, 0, 1, "unbound"); err == nil {
			t.Fatal("legacy Job became a file capability")
		}
		for _, bounds := range [][2]int{{-1, 1}, {0, 0}, {0, 52}, {1, 1}} {
			if _, err := tx.TraceRuns("p", 0, bounds[0], bounds[1], "unbound"); err == nil {
				t.Fatalf("invalid bounds accepted: %v", bounds)
			}
		}
	})
}
