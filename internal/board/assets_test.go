package board

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormalizeAssetEquivalentAddresses(t *testing.T) {
	for _, pair := range [][2]AssetSpec{
		{{Kind: "host", Value: "EXAMPLE.COM."}, {Kind: "host", Value: "example.com"}},
		{{Kind: "host", Value: "2001:0db8:0:0:0:0:0:1"}, {Kind: "host", Value: "2001:db8::1"}},
		{{Kind: "service", Value: "HTTP://EXAMPLE.COM.:080/"}, {Kind: "service", Value: "http://example.com"}},
		{{Kind: "service", Value: "https://[2001:0db8::1]:443/"}, {Kind: "service", Value: "https://[2001:db8::1]"}},
		{{Kind: "service", Value: "tcp://EXAMPLE.COM:0022"}, {Kind: "service", Value: "tcp://example.com:22"}},
		{{Kind: "endpoint", Value: "https://EXAMPLE.COM.:443", Method: "GET"}, {Kind: "endpoint", Value: "https://example.com/", Method: "GET"}},
	} {
		a, err := NormalizeAsset(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		b, err := NormalizeAsset(pair[1])
		if err != nil || a != b || !strings.HasPrefix(a.ID, "asset_") || len(a.ID) != 38 {
			t.Fatalf("equivalent addresses differ: %+v %+v (%v)", a, b, err)
		}
	}
}

func TestNormalizeAssetPreservesMethodPathAndQueryIdentity(t *testing.T) {
	seen := map[string]Asset{}
	for _, spec := range []AssetSpec{
		{Kind: "host", Value: "example.com"},
		{Kind: "service", Value: "https://example.com"},
		{Kind: "endpoint", Value: "https://example.com/Orders?a=1&b=2", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/orders?a=1&b=2", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/Orders?b=2&a=1", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/Orders?a=2&b=2", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/Orders?a=1&b=2", Method: "POST"},
		// HTTP method tokens are case sensitive, including extension methods.
		{Kind: "endpoint", Value: "https://example.com/Orders?a=1&b=2", Method: "get"},
		{Kind: "endpoint", Value: "https://example.com/%4frders?a=1&b=2", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/a%2fb", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/a/b", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/?", Method: "GET"},
	} {
		asset, err := NormalizeAsset(spec)
		if err != nil {
			t.Fatalf("%+v: %v", spec, err)
		}
		if previous, ok := seen[asset.ID]; ok {
			t.Fatalf("distinct targets merged: %+v and %+v", previous, asset)
		}
		seen[asset.ID] = asset
	}
}

func TestNormalizeAssetRejectsAmbiguousInputs(t *testing.T) {
	for _, spec := range []AssetSpec{
		{Kind: "company", Value: "example.com"},
		{Kind: "host", Value: "https://example.com"},
		{Kind: "host", Value: "example.com:443"},
		{Kind: "host", Value: "999.1.1.1"},
		{Kind: "host", Value: "127.0.0.01"},
		{Kind: "host", Value: "example..com"},
		{Kind: "host", Value: "a.-b.example"},
		{Kind: "host", Value: "fe80::1%eth0"},
		{Kind: "host", Value: "example.com", Method: "GET"},
		{Kind: "service", Value: "https://example.com/path"},
		{Kind: "service", Value: "https://example.com/?q=1"},
		{Kind: "service", Value: "https://example.com?"},
		{Kind: "service", Value: "tcp://example.com"},
		{Kind: "service", Value: "http://2001:db8::1"},
		{Kind: "service", Value: "http://[example.com]"},
		{Kind: "service", Value: "http://[127.0.0.1]"},
		{Kind: "service", Value: "http://example.com:"},
		{Kind: "service", Value: "http://example.com:0"},
		{Kind: "service", Value: "http://example.com:65536"},
		{Kind: "endpoint", Value: "http://example.com/"},
		{Kind: "endpoint", Value: "ftp://example.com/", Method: "GET"},
		{Kind: "endpoint", Value: "http://user:secret@example.com/", Method: "GET"},
		{Kind: "endpoint", Value: "http://example.com/#", Method: "GET"},
		{Kind: "endpoint", Value: "http://example.com/", Method: "GET POST"},
	} {
		if _, err := NormalizeAsset(spec); err == nil {
			t.Fatalf("accepted invalid asset: %+v", spec)
		}
	}
	if _, err := normalizeActionAssets(make([]AssetSpec, MaxActionAssets+1)); err == nil {
		t.Fatal("accepted excessive asset count")
	}
}

func TestAssetStepIdentityReplayAndSeparateStorage(t *testing.T) {
	f := newPlanFixture(t)
	input := stepInput("Inspect this target", []string{"origin"}, "goal", 0)
	input["assets"] = []AssetSpec{{Kind: "host", Value: "EXAMPLE.COM."}, {Kind: "service", Value: "https://example.com:443/"}, {Kind: "host", Value: "example.com"}}
	first := f.action("step", "first", input)
	if len(first.AssetIDs) != 2 {
		t.Fatal("receipt omitted normalized asset IDs")
	}
	if replay := f.action("step", "first", input); !reflect.DeepEqual(first, replay) {
		t.Fatal("same-key retry changed receipt")
	}
	input["assets"] = []AssetSpec{{Kind: "service", Value: "https://example.com"}, {Kind: "host", Value: "example.com"}}
	if duplicate := f.action("step", "equivalent", input); !duplicate.Unchanged || duplicate.ID != first.ID {
		t.Fatalf("equivalent target created duplicate Step: %+v", duplicate)
	}
	state := f.state()
	if len(state.Assets) != 2 || len(state.AssetAnchors) != 2 || len(state.AssetIDs("step", first.ID)) != 2 {
		t.Fatalf("lost or duplicated assets: %+v %+v", state.Assets, state.AssetAnchors)
	}
	input["assets"] = []AssetSpec{{Kind: "host", Value: "other.example.com"}}
	if other := f.action("step", "other", input); other.Unchanged || other.ID == first.ID {
		t.Fatal("different target was suppressed as a duplicate Step")
	}
	f.tx(func(tx *Tx) error {
		var raw string
		if err := tx.QueryRow("SELECT data FROM xloom_state WHERE project_id='proj_001'").Scan(&raw); err != nil {
			return err
		}
		if strings.Contains(raw, `"assets"`) || strings.Contains(raw, `"asset_anchors"`) || strings.Contains(raw, "example.com") {
			t.Fatal("asset catalog leaked into stateData")
		}
		return nil
	})
}

func TestAssetActionStrictValidationAndImmutability(t *testing.T) {
	f := newPlanFixture(t)
	id := f.action("step", "first", stepInput("Inspect", []string{"origin"}, "goal", 0)).ID
	for _, raw := range []string{
		`{"action":"add","description":"new","from":["origin"],"assets":[{"kind":"host","value":"example.com","unknown":1}]}`,
		`{"action":"priority","id":"` + id + `","priority":1,"assets":[]}`,
	} {
		before := f.state()
		err := f.store.Do(context.Background(), func(tx *Tx) error {
			_, err := tx.StateAction("proj_001", f.fence, StateAction{Op: "step", IdempotencyKey: "invalid", Payload: json.RawMessage(raw)})
			return err
		})
		requireAPIStatus(t, err, 422)
		if !reflect.DeepEqual(before, f.state()) {
			t.Fatal("rejected asset input mutated graph")
		}
	}
}

func TestAssetSnapshotProjectAndGenerationIsolation(t *testing.T) {
	f := newPlanFixture(t)
	input := stepInput("Inspect", []string{"origin"}, "goal", 0)
	input["assets"] = []AssetSpec{{Kind: "host", Value: "example.com"}}
	id := f.action("step", "before-restart", input).ID
	before := f.state()
	var snapshot *InputSnapshot
	f.tx(func(tx *Tx) (err error) {
		snapshot, err = tx.FreezeInput(before)
		return err
	})
	f.tx(func(tx *Tx) error {
		if err := tx.Save(Graph{Project: Project{ID: "other", Title: "other", Status: "active", CreatedAt: tx.Now}}); err != nil {
			return err
		}
		other, err := tx.State("other")
		if err != nil {
			return err
		}
		if len(other.Assets)+len(other.AssetAnchors) != 0 {
			t.Fatal("assets escaped project boundary")
		}
		_, err = tx.ReadInputSnapshot("other", snapshot.ID)
		requireAPIStatus(t, err, 404)
		_, err = tx.RestartProject("proj_001", nil)
		return err
	})
	if state := f.state(); len(state.Assets)+len(state.AssetAnchors) != 0 {
		t.Fatal("previous round anchors became current")
	}
	f.planner("after-restart")
	newID := f.action("step", "after-restart", input).ID
	if newID == id || f.state().Assets[0].ID != before.Assets[0].ID || len(f.state().AssetAnchors) != 1 {
		t.Fatal("restart failed to reuse identity with new round anchors")
	}
	f.tx(func(tx *Tx) error {
		frozen, err := tx.ReadInputSnapshot("proj_001", snapshot.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(frozen.Assets, before.Assets) || !reflect.DeepEqual(frozen.AssetAnchors, before.AssetAnchors) || DecisionStateVersion(frozen) != snapshot.StateVersion {
			t.Fatal("live asset writes changed immutable snapshot")
		}
		var raw []byte
		if err = tx.QueryRow("SELECT data FROM xloom_round_entries WHERE project_id='proj_001' AND generation=0 AND kind='state'").Scan(&raw); err != nil {
			return err
		}
		var archived State
		if err = json.Unmarshal(raw, &archived); err != nil {
			return err
		}
		if !reflect.DeepEqual(archived.AssetAnchors, before.AssetAnchors) {
			t.Fatal("round archive lost asset provenance")
		}
		var count int
		if err = tx.QueryRow("SELECT count(*) FROM xloom_assets WHERE project_id='proj_001'").Scan(&count); err == nil && count != 1 {
			t.Fatal("new round duplicated stable asset identity")
		}
		return err
	})
}

func TestAssetFailureRollsBackOriginalActionAndIndexes(t *testing.T) {
	f := newPlanFixture(t)
	f.tx(func(tx *Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_assets BEFORE INSERT ON xloom_asset_anchors BEGIN SELECT RAISE(ABORT,'unavailable'); END`)
		return err
	})
	input := stepInput("Inspect", []string{"origin"}, "goal", 0)
	input["assets"] = []AssetSpec{{Kind: "host", Value: "example.com"}}
	raw, _ := json.Marshal(input)
	err := f.store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.StateAction("proj_001", f.fence, StateAction{Op: "step", IdempotencyKey: "atomic", Payload: raw})
		if err == nil {
			t.Fatal("injected anchor failure accepted")
		}
		// The outer caller handles the error and commits unrelated work.
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if state := f.state(); len(state.Steps)+len(state.Assets)+len(state.AssetAnchors) != 0 || state.Revision != 0 {
		t.Fatal("failed anchor write left partial action")
	}
	f.tx(func(tx *Tx) error {
		var count int
		if err := tx.QueryRow("SELECT (SELECT count(*) FROM xloom_assets)+(SELECT count(*) FROM xloom_asset_anchors)+(SELECT count(*) FROM xloom_state_actions)").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("failed action leaked asset catalog or receipt")
		}
		_, err := tx.Exec("DROP TRIGGER reject_assets")
		return err
	})
	if result := f.action("step", "atomic", input); result.ID != "i001" {
		t.Fatal("failed action consumed Step identity")
	}
}

func TestAssetDecisionPreviewAndFailedBatchDoNotLeakIndexes(t *testing.T) {
	f := newStateBenchmarkFixture(t, stateBenchmarkSize{0, 128, 1}, 2)
	batch := DecisionBatch{ExpectedVersion: f.version, Actions: []DecisionAction{{Op: "step", Ref: "assetstep", Payload: json.RawMessage(`{"action":"add","from":["origin"],"description":"Inspect target","assets":[{"kind":"host","value":"example.com"}]}`)}}}
	if err := f.store.Do(context.Background(), func(tx *Tx) error {
		preview, err := tx.PreviewDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch)
		if err != nil {
			return err
		}
		var count int
		if err = tx.QueryRow("SELECT (SELECT count(*) FROM xloom_assets)+(SELECT count(*) FROM xloom_asset_anchors)").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("preview leaked asset rows")
		}
		invalid := batch
		invalid.Actions = append(append([]DecisionAction{}, batch.Actions...), DecisionAction{Op: "step", Payload: json.RawMessage(`{"action":"add","from":["missing"],"description":"Invalid"}`)})
		if _, err = tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, invalid); err == nil {
			t.Fatal("invalid batch accepted")
		}
		if err = tx.QueryRow("SELECT (SELECT count(*) FROM xloom_assets)+(SELECT count(*) FROM xloom_asset_anchors)").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("rejected batch leaked assets after caller handled error")
		}
		committed, err := tx.CommitDecision(stateBenchmarkProject, stateBenchmarkPlanner, batch)
		if err == nil && (committed.IDs["assetstep"] != preview.IDs["assetstep"] || committed.StateVersion != preview.StateVersion) {
			t.Fatal("preview asset snapshot differs from committed batch")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func assetFact(f *orchestrationFixture, e Execution, key, value string) string {
	f.t.Helper()
	return f.action(e.Fence(), "fact", "fact:"+key, map[string]any{
		"description": "Observed " + key, "scope": "controlled fixture", "observed_at": f.store.Now().Format(time.RFC3339),
		"evidence": []EvidenceRef{{RunID: e.Lease, Path: "evidence/" + key + ".txt", Excerpt: key}},
		"assets":   []AssetSpec{{Kind: "host", Value: value}},
	}, "").ID
}

func TestAssetFindingUsesCurrentSupportWithoutPromotingEvidence(t *testing.T) {
	f := newOrchestrationFixture(t)
	worker := f.worker("producer", "")
	oldFact := assetFact(f, worker, "old", "old.example.com")
	newFact := assetFact(f, worker, "new", "new.example.com")
	makeCandidate := func(key, fact, host string) string {
		return f.action(worker.Fence(), "candidate", key, map[string]any{
			"claim": "The fixture is reachable", "scope": "same condition", "status": "verified", "sources": []string{fact}, "reason": "Retained observation",
			"assets": []AssetSpec{{Kind: "host", Value: host}},
		}, "").ID
	}
	oldCandidate := makeCandidate("old-candidate", oldFact, "old-candidate.example.com")
	newCandidate := makeCandidate("new-candidate", newFact, "new-candidate.example.com")
	curator, input := f.curator("curation")
	f.action(curator.Fence(), "curate", "curate-current", CuratePayload{ThroughRevision: input.Revision,
		Relations: []CurateRelation{{Kind: "supersedes", Source: newFact, Target: oldFact, Reason: "Fresh observation"}},
		Groups:    []CurateGroup{{CandidateIDs: []string{oldCandidate, newCandidate}, Status: "verified", Reason: "Current supported result"}},
	}, DecisionStateVersion(input))
	state := f.state()
	want := append(state.AssetIDs("fact", newFact), state.AssetIDs("candidate", newCandidate)...)
	slices.Sort(want)
	if got := state.AssetIDs("finding", state.Findings[0].ID); !slices.Equal(got, want) {
		t.Fatalf("finding assets use obsolete support: got %v want %v", got, want)
	}
	if len(state.AssetIDs("fact", oldFact)) != 1 || len(state.AssetIDs("candidate", oldCandidate)) != 1 || !reflect.DeepEqual(state.Candidates, input.Candidates) {
		t.Fatal("current association erased original provenance")
	}

	// A source-free asset-bearing candidate remains tentative after curation.
	other := newOrchestrationFixture(t)
	producer := other.worker("tentative", "")
	candidate := other.action(producer.Fence(), "candidate", "tentative", map[string]any{
		"claim": "Unverified hint", "scope": "fixture", "reason": "Needs evidence", "assets": []AssetSpec{{Kind: "host", Value: "example.com"}},
	}, "").ID
	curator, input = other.curator("tentative-curation")
	other.curate(curator, input, CurateGroup{CandidateIDs: []string{candidate}, Status: "candidate", Reason: "Keep tentative"})
	state = other.state()
	if state.Findings[0].Status != "candidate" || state.Findings[0].SupportValid || len(state.AssetIDs("finding", state.Findings[0].ID)) != 1 {
		t.Fatal("asset association promoted confidence or lost tentative provenance")
	}
}

func TestConcurrentAssetActionsReuseProjectIdentity(t *testing.T) {
	f := newOrchestrationFixture(t)
	second, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.Now = f.store.Now
	const writers = 8
	errors := make(chan error, writers)
	var group sync.WaitGroup
	for n := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			store := f.store
			if n%2 == 1 {
				store = second
			}
			input := stepInput(fmt.Sprintf("Task %d", n), []string{"origin"}, "goal", 0)
			input["assets"] = []AssetSpec{{Kind: "host", Value: "EXAMPLE.COM."}}
			raw, _ := json.Marshal(input)
			errors <- store.Do(context.Background(), func(tx *Tx) error {
				_, err := tx.StateAction("p", f.planner, StateAction{Op: "step", IdempotencyKey: fmt.Sprint(n), Payload: raw})
				return err
			})
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	state := f.state()
	if len(state.Assets) != 1 || len(state.AssetAnchors) != writers || len(state.Steps) != writers {
		t.Fatal("concurrent writers lost anchors or duplicated identity")
	}
}

func TestAssetEmptyProjectionPreservesLegacyWireAndVersion(t *testing.T) {
	f := newPlanFixture(t)
	state := f.state()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"assets"`) || strings.Contains(string(raw), `"asset_anchors"`) {
		t.Fatal("new empty fields changed legacy wire snapshot")
	}
	version := DecisionStateVersion(state)
	state.Assets, state.AssetAnchors = []Asset{}, []AssetAnchor{}
	if empty, _ := json.Marshal(state); string(empty) != string(raw) || DecisionStateVersion(state) != version {
		t.Fatal("empty asset projection changed legacy snapshot identity")
	}
	asset, err := NormalizeAsset(AssetSpec{Kind: "host", Value: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	state.Assets = []Asset{asset}
	state.AssetAnchors = []AssetAnchor{{AssetID: asset.ID, NodeKind: "fact", NodeID: "origin"}}
	if DecisionStateVersion(state) == version {
		t.Fatal("asset projection is missing from snapshot version")
	}
}
