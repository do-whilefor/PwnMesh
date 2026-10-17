package board

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestGraphOnlyExportCannotSilentlyDropFGSHistory(t *testing.T) {
	if _, err := Export(Graph{}, "timeline"); err == nil {
		t.Fatal("timeline must require FGS state and events")
	}
}

func TestTimelinePreservesCorrectionsAndWithdrawnPlans(t *testing.T) {
	f := newOrchestrationFixture(t)
	producer := f.worker("timeline-producer", "")
	first, second := f.fact(producer, "first observation"), f.fact(producer, "controlled replacement")
	candidate := f.candidate(producer, first, "verified")
	curator, input := f.curator("initial-finding")
	f.curate(curator, input, CurateGroup{CandidateIDs: []string{candidate}, Status: "verified", Reason: "Initial observation"})
	finding := f.state().Findings[0]
	curator, input = f.curator("correction")
	f.action(curator.Fence(), "curate", "correction", CuratePayload{ThroughRevision: input.Revision, Groups: []CurateGroup{}, Relations: []CurateRelation{{Kind: "supersedes", Source: second, Target: first, Reason: "The controlled request supersedes the earlier observation"}}}, DecisionStateVersion(input))
	goal := f.action(f.planner, "goal", "goal", map[string]any{"action": "add", "condition": "WITHDRAWN_CONDITION"}, "")
	step := f.action(f.planner, "step", "step", stepInput("ABANDONED_DIRECTION", []string{"origin"}, goal.ID, 1), "")
	f.action(f.planner, "step", "priority", map[string]any{"action": "priority", "id": step.ID, "priority": 10, "reason": "REORDER_REASON"}, "")
	f.action(f.planner, "step", "abandon", map[string]any{"action": "abandon", "id": step.ID, "reason": "ABANDON_REASON"}, "")
	f.action(f.planner, "goal", "withdraw", map[string]any{"action": "withdraw", "id": goal.ID, "reason": "WITHDRAW_REASON"}, "")
	f.do(func(tx *Tx) error {
		timeline, err := tx.ExportTimeline("p")
		if err != nil {
			return err
		}
		for _, content := range []string{"first observation", "controlled replacement", "/workspace/timeline-producer/first observation.txt", "/workspace/timeline-producer/controlled replacement.txt", "FACT_RELATION", "supersedes", "WITHDRAWN_CONDITION", "WITHDRAW_REASON", "ABANDONED_DIRECTION", "ABANDON_REASON", "REORDER_REASON", "FACT " + first + " status=superseded", "FINDING " + finding.ID + " status=verified support_valid=false", "GOAL " + goal.ID + " status=withdrawn"} {
			if !strings.Contains(timeline, content) {
				t.Errorf("timeline omitted %q", content)
			}
		}
		if strings.Index(timeline, "REORDER_REASON") > strings.Index(timeline, "ABANDON_REASON") {
			t.Fatal("equal-timestamp mutations lost revision order")
		}
		return nil
	})
}

func TestTimelineContinuesAfterThousandEventsAndKeepsLegacyFacts(t *testing.T) {
	f := newPlanFixture(t)
	f.tx(func(tx *Tx) error {
		for n := 1; n <= 1001; n++ {
			result, _ := json.Marshal(map[string]string{"reason": fmt.Sprintf("event-%04d", n)})
			// Older diagnostic events can have a payload without a result.
			event, _ := json.Marshal(StateEvent{Revision: int64(n), Op: "execution_failed", ID: "legacy-step", CreatedAt: tx.Now, Payload: result})
			if _, err := tx.transactionQueries.Exec("INSERT INTO pwnmesh_state_events(project_id,revision,event) VALUES('proj_001',?,?)", n, string(event)); err != nil {
				return err
			}
		}
		timeline, err := tx.ExportTimeline("proj_001")
		if err == nil && (!strings.Contains(timeline, "event-1001") || !strings.Contains(timeline, "First observation") || !strings.Contains(timeline, "snapshot (no creation event recorded)")) {
			t.Fatal("timeline lost later events or pre-event legacy records")
		}
		return err
	})
}
