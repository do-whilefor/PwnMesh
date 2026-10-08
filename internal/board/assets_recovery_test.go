package board

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func reopenAssetFixture(t *testing.T, f *orchestrationFixture) {
	t.Helper()
	now := f.store.Now
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	store.Now = now
	f.store = store
}

func checkRecoveredAssetSnapshot(t *testing.T, f *orchestrationFixture, ref *InputSnapshot, want State) {
	t.Helper()
	wantRaw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	f.do(func(tx *Tx) error {
		got, err := tx.ReadInputSnapshot("p", ref.ID)
		if err != nil {
			return err
		}
		metadata, err := tx.InputSnapshotMetadata("p", ref.ID)
		if err != nil {
			return err
		}
		var raw []byte
		if err = tx.QueryRow("SELECT state FROM xloom_input_snapshots WHERE project_id=? AND id=?", "p", ref.ID).Scan(&raw); err != nil {
			return err
		}
		if !bytes.Equal(raw, wantRaw) || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(metadata, ref) || DecisionStateVersion(got) != ref.StateVersion {
			t.Fatal("reopening changed the frozen state, stored bytes, metadata or version")
		}
		return nil
	})
}

func TestAssetNodesReceiptsAndSnapshotSurviveStoreReopen(t *testing.T) {
	f := newOrchestrationFixture(t)
	assets := []AssetSpec{{Kind: "host", Value: "EXAMPLE.TEST."}, {Kind: "endpoint", Value: "https://example.test:443/Orders?id=7", Method: "GET"}}
	stepInput := map[string]any{"action": "add", "description": "Observe the controlled targets", "from": []string{"origin"}, "assets": assets}
	step := f.action(f.planner, "step", "recovered-step", stepInput, "")
	worker := f.worker("recovered-worker", step.ID)
	factInput := map[string]any{
		"description": "The controlled endpoint returned a response", "scope": "anonymous controlled request", "observed_at": f.store.Now().Format(time.RFC3339),
		"evidence": []EvidenceRef{{RunID: worker.Lease, Path: "evidence/response.txt", Excerpt: "HTTP 200"}}, "assets": assets,
	}
	fact := f.action(worker.Fence(), "fact", "recovered-fact", factInput, "")
	candidateInput := map[string]any{
		"claim": "The controlled endpoint is reachable", "scope": "anonymous controlled request", "status": "verified", "sources": []string{fact.ID}, "reason": "Retained response",
		"assets": []AssetSpec{{Kind: "host", Value: "example.test"}, {Kind: "service", Value: "https://example.test"}},
	}
	candidate := f.action(worker.Fence(), "candidate", "recovered-candidate", candidateInput, "")
	curator, input := f.curator("recovered-curator")
	f.curate(curator, input, CurateGroup{CandidateIDs: []string{candidate.ID}, Status: "verified", Reason: "The retained response supports the observation"})
	want := f.state()
	if len(want.Assets) != 3 || len(want.AssetAnchors) != 9 || len(want.AssetIDs("finding", want.Findings[0].ID)) != 3 {
		t.Fatalf("incomplete asset fixture: assets=%+v anchors=%+v", want.Assets, want.AssetAnchors)
	}
	var snapshot *InputSnapshot
	f.do(func(tx *Tx) (err error) {
		snapshot, err = tx.FreezeInput(want)
		return err
	})

	reopenAssetFixture(t, f)
	if got := f.state(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopening changed persisted asset nodes or finding projection: got=%+v want=%+v", got, want)
	}
	checkRecoveredAssetSnapshot(t, f, snapshot, want)
	for _, action := range []struct {
		fence   ExecutionFence
		op, key string
		payload any
		want    StateActionResult
	}{
		{f.planner, "step", "recovered-step", stepInput, step},
		{worker.Fence(), "fact", "recovered-fact", factInput, fact},
		{worker.Fence(), "candidate", "recovered-candidate", candidateInput, candidate},
	} {
		if got := f.action(action.fence, action.op, action.key, action.payload, ""); !reflect.DeepEqual(got, action.want) {
			t.Fatalf("reopening changed the %s idempotency receipt: got=%+v want=%+v", action.op, got, action.want)
		}
	}
	if !reflect.DeepEqual(f.state(), want) {
		t.Fatal("replaying restored receipts duplicated nodes or anchors")
	}
}

func TestAssetSchemaMigrationPreservesOldSnapshotAndPersistsNewAnchors(t *testing.T) {
	f := newOrchestrationFixture(t)
	worker := f.worker("migration-worker", "")
	oldFact := f.fact(worker, "before-assets")
	oldState := f.state()
	var oldSnapshot *InputSnapshot
	f.do(func(tx *Tx) (err error) {
		oldSnapshot, err = tx.FreezeInput(oldState)
		if err != nil {
			return err
		}
		// This is a pre-asset schema fixture, not a run of an older binary.
		// All retained business records and snapshot bytes remain untouched.
		if _, err = tx.Exec("DROP TABLE xloom_asset_anchors"); err != nil {
			return err
		}
		_, err = tx.Exec("DROP TABLE xloom_assets")
		return err
	})

	reopenAssetFixture(t, f)
	if got := f.state(); !reflect.DeepEqual(got, oldState) {
		t.Fatal("asset schema migration changed preexisting state or inferred historical anchors")
	}
	checkRecoveredAssetSnapshot(t, f, oldSnapshot, oldState)
	input := map[string]any{
		"description": "A new controlled response after migration", "scope": "anonymous controlled request", "observed_at": f.store.Now().Format(time.RFC3339),
		"evidence": []EvidenceRef{{RunID: worker.Lease, Path: "evidence/after-assets.txt", Excerpt: "HTTP 401"}},
		"assets": []AssetSpec{
			{Kind: "host", Value: "EXAMPLE.TEST."}, {Kind: "host", Value: "example.test"},
			{Kind: "endpoint", Value: "https://example.test/Orders?id=7", Method: "GET"},
			{Kind: "endpoint", Value: "https://example.test/Orders?id=7", Method: "POST"},
		},
	}
	result := f.action(worker.Fence(), "fact", "after-migration", input, "")
	want := f.state()
	if len(result.AssetIDs) != 3 || len(want.Assets) != 3 || len(want.AssetAnchors) != 3 || len(want.AssetIDs("fact", oldFact)) != 0 || len(want.AssetIDs("step", worker.Intent)) != 0 {
		t.Fatalf("upgraded store merged endpoints or inferred historical anchors: %+v", want.AssetAnchors)
	}
	var newSnapshot *InputSnapshot
	f.do(func(tx *Tx) (err error) {
		newSnapshot, err = tx.FreezeInput(want)
		return err
	})

	reopenAssetFixture(t, f)
	if !reflect.DeepEqual(f.state(), want) {
		t.Fatal("upgraded store lost the new multi-asset observation after reopening")
	}
	checkRecoveredAssetSnapshot(t, f, oldSnapshot, oldState)
	checkRecoveredAssetSnapshot(t, f, newSnapshot, want)
	if replay := f.action(worker.Fence(), "fact", "after-migration", input, ""); !reflect.DeepEqual(replay, result) || !reflect.DeepEqual(f.state(), want) {
		t.Fatal("upgraded store did not retain the asset-bearing idempotency receipt")
	}
}
