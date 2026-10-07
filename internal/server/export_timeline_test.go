package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"pwnmesh/internal/board"
)

func TestTimelineExportsProcessFactsFindingsAndFinalEvidence(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	live := true
	f.register("explore", &live, 2)
	fact := f.action("fact", "observed", map[string]any{"description": "PROCESS_OBSERVATION", "scope": "fixture", "observed_at": "2026-09-22T10:00:00Z", "evidence": []board.EvidenceRef{{RunID: f.run, Path: "process.txt", Excerpt: "PROCESS_EVIDENCE"}}})
	candidate := f.action("candidate", "finding", map[string]any{"claim": "VERIFIED_FINDING", "scope": "fixture", "status": "verified", "sources": []string{fact.ID}, "reason": "The observation supports this interpretation"})
	final := map[string]any{"description": "FINAL_OBSERVATION", "scope": "fixture", "observed_at": "2026-09-22T10:00:00Z", "evidence": []board.EvidenceRef{{RunID: f.run, Path: "final.txt", Excerpt: "FINAL_EVIDENCE"}}}
	raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": final}})
	f.pending(string(raw))
	f.apply(http.StatusOK)
	curator := *f
	curator.run, curator.lease = "export-curator", "planner@export-curator"
	_, curated := prepareCurator(&curator)
	curator.action("curate", "export-finding", map[string]any{"through_revision": curated.InputSnapshot.Revision, "groups": []any{map[string]any{"candidate_ids": []string{candidate.ID}, "status": "verified", "reason": "Evidence supports this finding"}}})
	timeline := f.request("GET", f.base()+"/export?format=timeline", nil, false, http.StatusOK, nil)
	for _, content := range []string{"PROCESS_OBSERVATION", "PROCESS_EVIDENCE", "VERIFIED_FINDING", "FINAL_OBSERVATION", "FINAL_EVIDENCE", "process.txt", "final.txt", "STEP_COMPLETED", "CURRENT FGS", "support_valid=true"} {
		if !strings.Contains(timeline, content) {
			t.Errorf("timeline omitted %q", content)
		}
	}
	if strings.Count(timeline, "FINAL_OBSERVATION") != 1 {
		t.Fatalf("final observation was exported twice: %s", timeline)
	}
	yaml := f.request("GET", f.base()+"/export?format=yaml", nil, false, http.StatusOK, nil)
	want, err := board.Export(f.state().Graph, "yaml")
	if err != nil || yaml != want {
		t.Fatalf("legacy YAML changed: %v", err)
	}
}
