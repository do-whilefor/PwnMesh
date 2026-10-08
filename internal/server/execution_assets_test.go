package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

func TestExecutionAssetsPublishFinishAndFrozenHTTPReads(t *testing.T) {
	publisher, _ := newSnapshotHTTPFixture(t)
	prepareSnapshot(t, publisher, snapshotTemplate(publisher, "explore"))
	fact := evidenceFixtureFact(publisher.run)
	fact["assets"] = []board.AssetSpec{
		{Kind: "host", Value: "EXAMPLE.TEST."},
		{Kind: "endpoint", Value: "https://EXAMPLE.TEST.:443/Orders?id=7", Method: "GET"},
		{Kind: "host", Value: "example.test"},
	}
	published := publisher.action("fact", "published-with-assets", fact)
	if len(published.AssetIDs) != 2 || !reflect.DeepEqual(published, publisher.action("fact", "published-with-assets", fact)) {
		t.Fatal("HTTP publication lost normalized asset IDs or changed its replay receipt")
	}
	host, err := board.NormalizeAsset(board.AssetSpec{Kind: "host", Value: "example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(published.AssetIDs, host.ID) {
		t.Fatal("equivalent host spellings did not share the receipt identity")
	}

	// A second real execution captures the published identities and anchors.
	reader := *publisher
	reader.run, reader.lease = "asset-reader", "planner@asset-reader"
	_, job := prepareSnapshot(t, &reader, snapshotTemplate(&reader, "explore"))
	read := worker.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_snapshot", Section: "assets", Limit: 1, ExpectedVersion: job.InputSnapshot.StateVersion}
	path := reader.base() + "/executions/" + reader.run + "/input/read"
	var first decisionReadPage
	reader.request("POST", path, read, true, http.StatusOK, &first)
	if first.Total != 2 || len(first.Items) != 1 || first.NextOffset == nil || first.StateVersion != job.InputSnapshot.StateVersion {
		t.Fatalf("prepared snapshot omitted the asset catalog: %+v", first)
	}

	// This is the normal finish_step result boundary: apply publishes its
	// inline Fact and completes the Step under the registered Explore lease.
	final := evidenceFixtureFact(publisher.run)
	final["description"] = "POST /Orders?id=7 returned 401 without credentials"
	final["assets"] = []board.AssetSpec{
		{Kind: "host", Value: "example.test"},
		{Kind: "endpoint", Value: "https://example.test/Orders?id=7", Method: "POST"},
	}
	output, err := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": final}})
	if err != nil {
		t.Fatal(err)
	}
	publisher.pending(string(output))
	publisher.apply(http.StatusOK)
	current := publisher.state()
	var finalID string
	for _, step := range current.Steps {
		if step.ID == publisher.intent {
			if step.Status != "completed" {
				t.Fatalf("asset-bearing result did not complete its Step: %+v", step)
			}
			finalID = board.Value(step.Result)
		}
	}
	if finalID == "" || finalID == published.ID || len(current.Assets) != 3 || len(current.AssetAnchors) != 4 || len(current.AssetIDs("fact", finalID)) != 2 || !slices.Equal(current.AssetIDs("fact", published.ID), published.AssetIDs) {
		t.Fatalf("finish lost anchors or merged GET/POST identities: assets=%+v anchors=%+v", current.Assets, current.AssetAnchors)
	}
	for _, record := range current.FactRecords {
		if record.ID == finalID && (record.SourceStepID != publisher.intent || record.RunID != publisher.lease || record.Legacy || len(record.Evidence) != 1) {
			t.Fatalf("final asset association replaced original evidence provenance: %+v", record)
		}
	}
	publisher.apply(http.StatusOK)
	if !reflect.DeepEqual(current, publisher.state()) {
		t.Fatal("completion replay duplicated asset anchors or the final observation")
	}

	// Resume a page after the live catalog changed. Both identities and their
	// anchors must still come from the original immutable execution input.
	read.Offset = *first.NextOffset
	var second decisionReadPage
	reader.request("POST", path, read, true, http.StatusOK, &second)
	if second.Total != 2 || len(second.Items) != 1 || second.NextOffset != nil || second.StateVersion != first.StateVersion || second.Revision != first.Revision {
		t.Fatalf("asset pagination crossed snapshot versions: first=%+v second=%+v", first, second)
	}
	var frozenIDs []string
	for _, item := range append(first.Items, second.Items...) {
		var asset board.Asset
		if err = json.Unmarshal(item, &asset); err != nil {
			t.Fatal(err)
		}
		frozenIDs = append(frozenIDs, asset.ID)
	}
	slices.Sort(frozenIDs)
	if !slices.Equal(frozenIDs, published.AssetIDs) {
		t.Fatal("frozen catalog substituted a later endpoint")
	}
	read.Section, read.Offset, read.Limit = "anchors", 0, 20
	var anchors decisionReadPage
	reader.request("POST", path, read, true, http.StatusOK, &anchors)
	if anchors.Total != 2 || anchors.StateVersion != first.StateVersion {
		t.Fatal("frozen anchor index mixed in live writes")
	}
	for _, item := range anchors.Items {
		var anchor board.AssetAnchor
		if err = json.Unmarshal(item, &anchor); err != nil {
			t.Fatal(err)
		}
		if anchor.NodeKind != "fact" || anchor.NodeID != published.ID || !slices.Contains(frozenIDs, anchor.AssetID) {
			t.Fatalf("frozen anchor targets another observation or catalog: %+v", anchor)
		}
	}
	read.Section, read.AssetIDs = "facts", []string{host.ID}
	var frozen decisionReadPage
	reader.request("POST", path, read, true, http.StatusOK, &frozen)
	if frozen.Total != 1 || len(frozen.Items) != 1 || frozen.StateVersion != first.StateVersion {
		t.Fatal("asset-filtered snapshot observed a later final Fact")
	}
	var original board.FactRecord
	if err = json.Unmarshal(frozen.Items[0], &original); err != nil || original.ID != published.ID {
		t.Fatal("asset-filtered frozen read lost its original observation")
	}
	read.Op, read.ExpectedVersion = "read_graph", ""
	var live decisionReadPage
	reader.request("POST", reader.base()+"/state/read", read, true, http.StatusOK, &live)
	if live.Total != 2 || live.StateVersion != board.DecisionStateVersion(current) || live.StateVersion == frozen.StateVersion {
		t.Fatalf("live asset filter did not expose both independently retained Facts: %+v", live)
	}
}
