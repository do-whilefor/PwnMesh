package board

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestAppendProjectionMatchesPersistedState(t *testing.T) {
	f := newOrchestrationFixture(t)
	run := f.worker("producer", "")
	source := f.fact(run, "original")
	assets := []AssetSpec{{Kind: "host", Value: "example.test"}}
	var derived string
	for _, op := range []string{"fact", "candidate"} {
		payload := map[string]any{"description": "Derived observation", "scope": "controlled fixture", "observed_at": f.store.Now().Format(time.RFC3339), "derived_from": []string{source}, "assets": assets,
			"evidence": []EvidenceRef{{RunID: run.Lease, Path: "/workspace/producer/derived.txt", Excerpt: "computed from original"}}}
		if op == "candidate" {
			payload = map[string]any{"claim": "The result is present", "scope": "controlled fixture", "status": "verified", "sources": []string{derived}, "reason": "Computed result retained", "assets": assets}
		}
		raw, _ := json.Marshal(payload)
		f.do(func(tx *Tx) error {
			snapshot, err := tx.State("p")
			if err != nil {
				return err
			}
			out, err := tx.stateAction(&snapshot, run.Fence(), StateAction{Op: op, IdempotencyKey: "append:" + op, Payload: raw}, directStateAction)
			if err != nil {
				return err
			}
			persisted, err := tx.State("p")
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(snapshot, persisted) || out.StateVersion != DecisionStateVersion(persisted) || !reflect.DeepEqual(out.AssetIDs, persisted.AssetIDs(op, out.ID)) {
				t.Fatalf("%s append projection differs from durable readback", op)
			}
			if op == "fact" {
				derived = out.ID
			}
			return nil
		})
	}
}
