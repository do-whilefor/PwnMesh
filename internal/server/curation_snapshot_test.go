package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
	"strings"
	"testing"
)

func TestLargeCurationUsesLosslessSnapshotAndRetainsCommitFence(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%t", stale), func(t *testing.T) {
			f := newCurationHTTPFixture(t)
			prepareOrchestratedExecute(f)
			want := map[string]string{}
			last := ""
			for n := 0; n < 12; n++ {
				payload := evidenceFixtureFact(f.run)
				payload["description"] = fmt.Sprintf("Observation %d: ", n) + strings.Repeat("x", 8192)
				fact := f.action("fact", fmt.Sprintf("observation-%d", n), payload)
				last = fact.ID
				want[fact.ID] = payload["description"].(string)
			}
			f.pending(fmt.Sprintf(`{"accepted":true,"outcome":"completed","data":{"fact_id":%q}}`, last))
			f.apply(http.StatusOK)
			full, _ := json.Marshal(f.state())
			// The old envelope repeated Graph and the complete State.
			legacy, _ := json.Marshal(worker.Job{Graph: f.state().Graph, State: func() *board.State { s := f.state(); return &s }()})
			if len(legacy) <= board.MaxCurationInputBytes {
				t.Fatal("fixture does not exceed the old immutable input limit")
			}
			f.run, f.lease = "large-curator", "planner@large-curator"
			e, job := prepareCurator(f)
			if job.State != nil || job.InputSnapshot == nil || len(e.Job) >= board.MaxCurationInputBytes || len(job.InputView) > board.DefaultContextViewBytes || len(e.Job) >= len(full) {
				t.Fatal("curation still transports full state")
			}
			if stale {
				f.request("POST", f.base()+"/hints", map[string]string{"content": "A newer requirement", "creator": "fixture"}, false, http.StatusCreated, nil)
			}
			seen := map[string]bool{}
			for offset := 0; ; {
				var page struct {
					Items        []board.FactRecord `json:"items"`
					NextOffset   int                `json:"next_offset"`
					StateVersion string             `json:"state_version"`
				}
				f.request("POST", f.base()+"/executions/"+f.run+"/input/read", worker.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_snapshot", Section: "facts", Offset: offset, Limit: 2, ExpectedVersion: job.InputSnapshot.StateVersion}, true, http.StatusOK, &page)
				if page.StateVersion != job.InputSnapshot.StateVersion {
					t.Fatal("snapshot read refreshed the input boundary")
				}
				for _, fact := range page.Items {
					if text, ok := want[fact.ID]; ok {
						if fact.Description != text || len(fact.Evidence) == 0 {
							t.Fatal("snapshot lost original observation bytes or evidence")
						}
						seen[fact.ID] = true
					}
				}
				if page.NextOffset == 0 {
					break
				}
				if page.NextOffset <= offset {
					t.Fatal("cursor did not advance")
				}
				offset = page.NextOffset
			}
			if len(seen) != len(want) {
				t.Fatalf("snapshot retained %d/%d observations", len(seen), len(want))
			}
			body := map[string]any{"op": "curate", "idempotency_key": "large-curation", "expected_version": job.InputSnapshot.StateVersion, "payload": board.CuratePayload{ThroughRevision: job.InputSnapshot.Revision, Groups: []board.CurateGroup{}}}
			if stale {
				f.request("POST", f.base()+"/state/actions", body, true, http.StatusConflict, nil)
				f.request("GET", f.base()+"/state/curation/receipt", nil, true, http.StatusNotFound, nil)
				return
			}
			f.request("POST", f.base()+"/state/actions", body, true, http.StatusOK, nil)
			f.pending(`{"accepted":true,"data":{"curated":true}}`)
			f.apply(http.StatusOK)
			if current := f.state(); current.Curation.ThroughRevision != job.InputSnapshot.Revision || len(current.FactRecords) != len(want)+2 {
				t.Fatal("large curation lost observations or failed to advance its cursor")
			}
		})
	}
}
