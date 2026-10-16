package board

import (
	"reflect"
	"testing"
)

func TestStepResultAssetDiscoverySurvivesSnapshotAndRestart(t *testing.T) {
	f := newOrchestrationFixture(t)
	step := f.action(f.planner, "step", "discovery-step", map[string]any{
		"action": "add", "description": "Observe the declared target", "from": []string{"origin"},
		"assets": []AssetSpec{{Kind: "host", Value: "example.test"}},
	}, "")
	run := f.worker("discovery-worker", step.ID)
	fact := f.fact(run, "untagged-result")
	before := f.state()
	if len(before.AssetIDs("fact", fact)) != 0 {
		t.Fatal("unfinished observations inherited task targets")
	}
	f.finish(run, fact)
	want := f.state()
	if !reflect.DeepEqual(want.AssetIDs("fact", fact), step.AssetIDs) {
		t.Fatal("completed result is absent from declared asset discovery")
	}
	found := false
	for _, anchor := range want.AssetAnchors {
		if anchor.NodeKind == "fact" && anchor.NodeID == fact {
			found = anchor.DiscoveryStepID == step.ID
		}
	}
	if !found {
		t.Fatal("result discovery omitted producing Step provenance")
	}
	var snapshot *InputSnapshot
	f.do(func(tx *Tx) (err error) {
		var stored int
		if err = tx.transactionQueries.QueryRow("SELECT count(*) FROM pwnmesh_asset_anchors WHERE project_id=? AND node_kind='fact' AND node_id=?", "p", fact).Scan(&stored); err != nil {
			return err
		}
		if stored != 0 {
			t.Fatal("derived discovery was stored as an observed anchor")
		}
		snapshot, err = tx.FreezeInput(want)
		return err
	})
	reopenAssetFixture(t, f)
	if !reflect.DeepEqual(f.state(), want) {
		t.Fatal("store reopen changed derived discovery")
	}
	checkRecoveredAssetSnapshot(t, f, snapshot, want)
	f.do(func(tx *Tx) error { _, err := tx.RestartProject("p", nil); return err })
	if current := f.state(); len(current.Assets)+len(current.AssetAnchors) != 0 {
		t.Fatal("old discovery escaped its generation")
	}
	checkRecoveredAssetSnapshot(t, f, snapshot, want)
}
