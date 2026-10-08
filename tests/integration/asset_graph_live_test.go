//go:build linux

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
	"pwnmesh/internal/workergraph"
)

const assetProducerScope = "asset-producer-v1"
const assetConsumerScope = "asset-consumer-v1"
const assetConsumerPath = "/workspace/asset-consumer.json"

// The same production lifecycle as the other live workloads, with independent
// acceptance of graph receipts, retained bytes and cross-Worker observations.
func TestLiveAssetGraphProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_ASSET_GRAPH_TEST") != "1" {
		t.Skip("opt in to real model and Docker asset graph acceptance")
	}
	origin := `Use ONLY synthetic local files under /workspace. The example.test addresses below are identity labels: never contact them, install packages or read credentials.
Authorize exactly two ordinary Steps in sequence. First authorize only the producer. After it successfully completes, authorize the consumer from the producer's accepted fact. Preserve these exact execution contracts in their tasks. Both Steps must have assets [{"kind":"host","value":"EXAMPLE.TEST."},{"kind":"service","value":"https://EXAMPLE.TEST.:443"},{"kind":"endpoint","value":"https://EXAMPLE.TEST.:443/Orders?id=7","method":"GET"}]. No curation, candidate, dispute or extra Step is needed.
PRODUCER: use run_graph with key asset-seed and exactly two required command nodes, resources:[]: seed, then receipt with depends_on:[{"id":"seed"}]. seed uses Python secrets.token_hex(16) and prints exactly one JSON object {"nonce":the generated 32-character hexadecimal string,"values":[2,3,5]}. receipt reads the seed's actual retained stdout.log through the PWNMESH_DEPENDENCIES JSON FILE, computes the sum and SHA-256 of the exact seed stdout bytes, then prints exactly one JSON object {"nonce":the read nonce,"sum":the calculated sum,"seed_sha256":the calculated hash}. Do not hardcode nonce, sum or hash. Preserve both nodes' stdout and stderr. After run_graph succeeds, use bash to read receipt stdout and print ASSET_TRACE: followed by its nonce. Publish exactly one fact with scope asset-producer-v1, original evidence from BOTH node stdout paths and the same three assets as the producer Step. Describe only the local computation, without copying ASSET_TRACE or its nonce into the description. Finish with this fact_id.
CONSUMER: the planner must keep this consumer Step's assets limited to the SAME THREE identities as the producer Step (host, service and GET endpoint); POST belongs ONLY to the consumer Fact published later, never to either planned Step. Before publishing anything, read_snapshot section assets limit 1, retain its state_version and next_offset, and leave the remaining pages unread for now. Use read_graph section assets to discover the normalized GET endpoint ID; then read_graph section facts with asset_ids:[that ID] and verify the producer fact is returned. Use read_worker_trace without run_id to list other runs, find the producer, search that run for ASSET_TRACE:, and read a returned matching bash record using its record and trace_version. This is an unverified clue: independently read the producer's original seed and receipt evidence files, recompute the sum and seed SHA-256, and compare the actual nonce to the trace observation. Write AND print /workspace/asset-consumer.json as {"nonce":the independently read nonce,"sum":the independently computed sum,"seed_sha256":the independently computed hash}. Publish exactly one fact with scope asset-consumer-v1 and evidence from this consumer receipt and BOTH original producer stdout files. Use assets [{"kind":"host","value":"example.test"},{"kind":"service","value":"https://example.test"},{"kind":"endpoint","value":"https://example.test/Orders?id=7","method":"GET"},{"kind":"endpoint","value":"https://example.test/Orders?id=7","method":"POST"}]. This is a local fixture observation, not a claim about a live service.
AFTER publishing but BEFORE finishing, continue the earlier read_snapshot assets page with offset:the returned next_offset, limit:1 and expected_version:the earlier state_version until no next_offset remains. Read read_snapshot anchors using the same expected_version. Verify the frozen catalog still has exactly three identities and its anchors exclude your newly published fact. Read live read_graph assets and anchors to verify four identities, separate GET/POST and your new fact's anchors. Finish with the already published consumer fact_id. The main Agent completes only after both successful Steps, citing the consumer fact and its retained evidence.`
	runObservedProject(t, "Asset identity, Worker DAG and cross-Worker trace acceptance", origin,
		"Complete the two synthetic Steps with normalized asset anchors, a real dependency graph, cross-Worker trace retrieval and immutable snapshot reads across publication.",
		"asset_graph_worker_trace_and_frozen_input", validateAssetGraphDelivery, 1)
}

// Revalidate original live evidence after a validator change without another
// model call or any mutation of the retained state and workspace archive.
func TestRetainedAssetGraphProject(t *testing.T) {
	dir := os.Getenv("PWNMESH_ASSET_GRAPH_REPLAY_DIR")
	if dir == "" {
		t.Skip("opt in with the directory containing retained state.json and workspace.tar")
	}
	rawState, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state board.State
	if err = json.Unmarshal(rawState, &state); err != nil {
		t.Fatal(err)
	}
	archive, err := os.Open(filepath.Join(dir, "workspace.tar"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	digest := sha256.New()
	files, err := retainLiveWorkspace(io.TeeReader(archive, digest), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("validation_scope=asset_graph_worker_trace_and_frozen_input workspace_sha256=%x state_sha256=%x", digest.Sum(nil), sha256.Sum256(rawState))
	if failures := validateAssetGraphDelivery(state, files); len(failures) != 0 {
		t.Fatal(failures)
	}
}

type assetToolReceipt struct {
	Name   string
	Input  json.RawMessage
	Output []byte
	Index  int
}

// Only a matching successful result in the durable journal counts. A request,
// final prose, failed call or JSON file alone cannot prove a tool was executed.
func assetToolReceipts(raw []byte) []assetToolReceipt {
	calls := map[string]assetToolReceipt{}
	var receipts []assetToolReceipt
	for index, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event agent.Event
		if json.Unmarshal(line, &event) != nil {
			return nil
		}
		if event.Message == nil {
			continue
		}
		for _, block := range event.Message.Content {
			if block.Type == "tool_use" && event.Message.Role == "assistant" {
				calls[block.ID] = assetToolReceipt{Name: block.Name, Input: block.Input, Index: index}
			}
			call, exists := calls[block.ToolUseID]
			if block.Type != "tool_result" || !exists || event.Message.Role != "user" {
				continue
			}
			delete(calls, block.ToolUseID)
			var output string
			if !block.IsError && json.Unmarshal(block.Content, &output) == nil {
				call.Output, call.Index = []byte(output), index
				receipts = append(receipts, call)
			}
		}
	}
	return receipts
}

func assetFixtureIdentities() []board.Asset {
	var assets []board.Asset
	for _, spec := range []board.AssetSpec{{Kind: "host", Value: "example.test"}, {Kind: "service", Value: "https://example.test"}, {Kind: "endpoint", Value: "https://example.test/Orders?id=7", Method: "GET"}, {Kind: "endpoint", Value: "https://example.test/Orders?id=7", Method: "POST"}} {
		asset, err := board.NormalizeAsset(spec)
		if err != nil {
			panic(err)
		}
		assets = append(assets, asset)
	}
	return assets
}

func assetIDs(assets []board.Asset) []string {
	ids := make([]string, len(assets))
	for i, asset := range assets {
		ids[i] = asset.ID
	}
	slices.Sort(ids)
	return ids
}

type assetCalculation struct {
	Nonce  string `json:"nonce"`
	Sum    int    `json:"sum"`
	SHA256 string `json:"seed_sha256"`
}

func validateAssetGraphDelivery(state board.State, files map[string][]byte) []string {
	var failures []string
	check := func(ok bool, message string) {
		if !ok {
			failures = append(failures, message)
		}
	}
	check(state.Graph.Project.Status == "completed" && state.Graph.Project.OrchestrationVersion == 1, "project did not complete under orchestration protocol 1")
	want := assetFixtureIdentities()
	check(len(state.Assets) == 4, "normalized catalog does not contain exactly host/service/GET/POST")
	for _, asset := range want {
		check(slices.Contains(state.Assets, asset), "missing canonical identity: "+asset.Kind+" "+asset.Method)
	}
	var producer, consumer board.FactRecord
	facts := map[string]board.FactRecord{}
	for _, fact := range state.FactRecords {
		facts[fact.ID] = fact
		if fact.Scope == assetProducerScope {
			check(producer.ID == "", "duplicate producer observation")
			producer = fact
		}
		if fact.Scope == assetConsumerScope {
			check(consumer.ID == "", "duplicate consumer observation")
			consumer = fact
		}
	}
	check(producer.ID != "" && consumer.ID != "" && producer.ID != consumer.ID, "missing distinct producer and consumer facts")
	if producer.ID == "" || consumer.ID == "" {
		return failures
	}
	check(producer.RunID != "" && producer.RunID != consumer.RunID && producer.SourceStepID != consumer.SourceStepID, "cross-Worker observation reused the same run or Step")
	check(!strings.Contains(producer.Description, "ASSET_TRACE:"), "trace marker was promoted into the producer description")
	check(slices.Equal(state.AssetIDs("fact", producer.ID), assetIDs(want[:3])), "producer Fact lost normalized multi-asset anchors")
	check(slices.Equal(state.AssetIDs("fact", consumer.ID), assetIDs(want)), "consumer Fact lost shared anchors or merged GET and POST")
	check(len(state.AssetAnchors) == 13, "asset index contains unexpected or missing anchors")
	var consumerJob worker.Job
	ordinary := 0
	for _, step := range state.Steps {
		if staleRepairCompletionMarker(state, step) {
			continue
		}
		ordinary++
		fact := producer
		if step.ID == consumer.SourceStepID {
			fact = consumer
		}
		job, ok := orchestrationSuccessfulRun(state.Graph.Project, step, fact.RunID, facts, files)
		check(ok && step.ID == fact.SourceStepID && step.Result != nil && *step.Result == fact.ID, "Step lacks a successful original Worker result: "+step.ID)
		check(slices.Equal(state.AssetIDs("step", step.ID), assetIDs(want[:3])), "Step lost its three authorized asset anchors: "+step.ID)
		if step.ID == consumer.SourceStepID {
			consumerJob = job
			check(slices.Contains(step.From, producer.ID), "consumer was not authorized from producer's accepted fact")
		}
	}
	check(ordinary == 2, "expected exactly two ordinary Steps")
	check(slices.ContainsFunc(state.Goals, func(g board.Goal) bool {
		return g.ID == "goal" && g.Status == "achieved" && g.SupportValid && slices.Contains(g.Sources, consumer.ID)
	}), "root completion lacks valid consumer support")
	check(len(state.Candidates) == 0 && len(state.Disputes) == 0 && len(state.FactRelations) == 0, "simple asset workload created unrelated judgment work")
	producerRun := orchestrationRunID(producer.RunID)
	base := "/workspace/.pwnmesh/runs/" + producerRun + "/graph-tools/asset-seed/"
	seedRaw, receiptRaw := files[base+"nodes/seed/stdout.log"], files[base+"nodes/receipt/stdout.log"]
	var seed struct {
		Nonce  string `json:"nonce"`
		Values []int  `json:"values"`
	}
	var receipt, calculated assetCalculation
	seedOK := json.Unmarshal(seedRaw, &seed) == nil && slices.Equal(seed.Values, []int{2, 3, 5})
	nonce, nonceErr := hex.DecodeString(seed.Nonce)
	check(seedOK && nonceErr == nil && len(nonce) == 16, "DAG seed did not retain a generated nonce and actual input values")
	expected := assetCalculation{Nonce: seed.Nonce, Sum: 10, SHA256: fmt.Sprintf("%x", sha256.Sum256(seedRaw))}
	check(json.Unmarshal(receiptRaw, &receipt) == nil && receipt == expected, "DAG receipt differs from independently recomputed seed")
	check(json.Unmarshal(files[assetConsumerPath], &calculated) == nil && calculated == expected, "consumer did not independently reproduce nonce, sum and seed digest")
	for _, fact := range []board.FactRecord{producer, consumer} {
		for _, ref := range fact.Evidence {
			check(orchestrationEvidenceValid(ref, fact.RunID, files), "retained evidence failed original hash/provenance: "+fact.ID)
		}
		for _, raw := range [][]byte{seedRaw, receiptRaw} {
			check(len(raw) > 0 && slices.ContainsFunc(fact.Evidence, func(ref board.EvidenceRef) bool { return bytes.Equal(files[ref.Path], raw) }), "Fact lacks original DAG evidence: "+fact.ID)
		}
	}
	check(slices.ContainsFunc(consumer.Evidence, func(ref board.EvidenceRef) bool { return bytes.Equal(files[ref.Path], files[assetConsumerPath]) }), "consumer result lacks its own calculation evidence")
	check(orchestrationCommandOutput(consumer.RunID, files[assetConsumerPath], files), "consumer receipt was not printed by a successful local command")
	producerCalls := assetToolReceipts(files["/workspace/.pwnmesh/runs/"+producerRun+"/events.jsonl"])
	consumerCalls := assetToolReceipts(files["/workspace/.pwnmesh/runs/"+orchestrationRunID(consumer.RunID)+"/events.jsonl"])
	failures = append(failures, validateAssetDAG(producerRun, base, files, producerCalls)...)
	check(slices.ContainsFunc(producerCalls, func(call assetToolReceipt) bool {
		return call.Name == "bash" && bytes.Contains(call.Output, []byte("ASSET_TRACE:"+seed.Nonce))
	}), "producer did not emit the trace observation")
	failures = append(failures, validateAssetConsumerReads(consumerJob, producer, consumer, seed.Nonce, consumerCalls)...)
	return failures
}

func validateAssetDAG(run, base string, files map[string][]byte, calls []assetToolReceipt) []string {
	var failures []string
	check := func(ok bool, message string) {
		if !ok {
			failures = append(failures, message)
		}
	}
	var cp workergraph.Checkpoint
	if json.Unmarshal(files[base+"graph.json"], &cp) != nil {
		return []string{"missing retained producer DAG checkpoint"}
	}
	check(cp.SchemaVersion == 1 && cp.RunID == run && cp.Status == "succeeded" && len(cp.Nodes) == 2, "invalid producer DAG identity or completion")
	nodes := map[string]workergraph.NodeState{}
	for _, node := range cp.Nodes {
		check(nodes[node.ID].ID == "" && slices.Contains([]string{"seed", "receipt"}, node.ID), "duplicate or unexpected DAG node")
		nodes[node.ID] = node
		var value struct {
			OutputPath string `json:"output_path"`
			StderrPath string `json:"stderr_path"`
			ExitCode   int    `json:"exit_code"`
		}
		check(node.Kind == "function" && node.Status == "succeeded" && node.Attempt == 1 && !node.StartedAt.IsZero() && node.FinishedAt.After(node.StartedAt), "DAG node lacks a successful execution interval")
		check(json.Unmarshal(node.Output.Value, &value) == nil && value.ExitCode == 0 && value.OutputPath == base+"nodes/"+node.ID+"/stdout.log" && value.StderrPath == base+"nodes/"+node.ID+"/stderr.log", "DAG node output identity differs")
		for _, path := range []string{value.OutputPath, value.StderrPath} {
			raw, exists := files[path]
			check(exists && slices.Contains(node.Output.Artifacts, workergraph.Artifact{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}), "DAG artifact missing or hash differs")
		}
	}
	check(!nodes["receipt"].StartedAt.Before(nodes["seed"].FinishedAt), "DAG dependent ran before seed completion")
	var deps []workergraph.NodeState
	check(json.Unmarshal(files[base+"nodes/receipt/dependencies.json"], &deps) == nil && len(deps) == 1 && reflect.DeepEqual(deps[0], nodes["seed"]), "DAG dependency receipt differs from actual seed output")
	matched := false
	for _, call := range calls {
		if call.Name != "run_graph" {
			continue
		}
		var request struct {
			Key   string
			Nodes []struct {
				ID        string
				Command   string
				Optional  bool
				DependsOn []workergraph.Dependency `json:"depends_on"`
			}
		}
		var receipt struct {
			Key, Status string
			Nodes       []workergraph.NodeState
		}
		if json.Unmarshal(call.Input, &request) != nil || json.Unmarshal(call.Output, &receipt) != nil || request.Key != "asset-seed" || receipt.Key != request.Key || receipt.Status != "succeeded" || len(request.Nodes) != 2 || !reflect.DeepEqual(receipt.Nodes, cp.Nodes) {
			continue
		}
		valid := true
		seen := map[string]bool{}
		for _, node := range request.Nodes {
			valid = valid && !seen[node.ID] && !node.Optional && node.Command != ""
			seen[node.ID] = true
			if node.ID == "seed" {
				valid = valid && len(node.DependsOn) == 0
			} else {
				valid = valid && node.ID == "receipt" && slices.Equal(node.DependsOn, []workergraph.Dependency{{ID: "seed"}})
			}
		}
		matched = matched || valid
	}
	check(matched, "DAG has no matching successful run_graph request and receipt")
	return failures
}

func validateAssetConsumerReads(job worker.Job, producer, consumer board.FactRecord, nonce string, calls []assetToolReceipt) []string {
	var failures []string
	check := func(ok bool, message string) {
		if !ok {
			failures = append(failures, message)
		}
	}
	if job.InputSnapshot == nil {
		return []string{"consumer has no immutable execution input"}
	}
	want := assetFixtureIdentities()
	publication := -1
	for _, call := range calls {
		var action board.StateAction
		var receipt board.StateActionResult
		if call.Name == "graph_action" && json.Unmarshal(call.Input, &action) == nil && action.Op == "fact" && json.Unmarshal(call.Output, &receipt) == nil && receipt.Op == "fact" && receipt.ID == consumer.ID && receipt.Revision > 0 && receipt.StateVersion != "" {
			publication = call.Index
		}
	}
	check(publication >= 0, "consumer has no successful Fact publication receipt")
	discovered, filtered, listed, searched, detailed, liveAssets, liveAnchors, frozenAnchors := false, false, false, false, false, false, false, false
	searchRecords := map[string]string{}
	frozen := map[int]board.Asset{}
	for _, call := range calls {
		if call.Name == "read_worker_trace" {
			if call.Index >= publication {
				continue
			}
			var query struct {
				RunID, Query, Record, TraceVersion string
				ByteOffset                         int
			}
			// These field names have underscores in the public protocol.
			var args map[string]json.RawMessage
			_ = json.Unmarshal(call.Input, &args)
			_ = json.Unmarshal(args["run_id"], &query.RunID)
			_ = json.Unmarshal(args["query"], &query.Query)
			_ = json.Unmarshal(args["record"], &query.Record)
			_ = json.Unmarshal(args["trace_version"], &query.TraceVersion)
			_ = json.Unmarshal(args["byte_offset"], &query.ByteOffset)
			var result struct {
				RunID           string `json:"run_id"`
				StepID          string `json:"step_id"`
				TraceVersion    string `json:"trace_version"`
				Record, Content string
				Projection      bool
				Items           []struct {
					RunID                 string `json:"run_id"`
					StepID                string `json:"step_id"`
					Record, Tool, Content string
				}
			}
			if json.Unmarshal(call.Output, &result) != nil {
				continue
			}
			if query.RunID == "" {
				listed = listed || slices.ContainsFunc(result.Items, func(item struct {
					RunID                 string `json:"run_id"`
					StepID                string `json:"step_id"`
					Record, Tool, Content string
				}) bool {
					return item.RunID == orchestrationRunID(producer.RunID) && item.StepID == producer.SourceStepID
				})
			}
			if query.RunID != orchestrationRunID(producer.RunID) || result.RunID != query.RunID || result.StepID != producer.SourceStepID || !result.Projection || result.TraceVersion == "" {
				continue
			}
			if query.Record == "" && query.Query == "ASSET_TRACE:" {
				for _, item := range result.Items {
					if item.Tool == "bash" && strings.Contains(item.Content, "ASSET_TRACE:"+nonce) && item.Record != "" {
						searched = true
						searchRecords[item.Record] = result.TraceVersion
					}
				}
			} else if query.Record != "" && searchRecords[query.Record] == query.TraceVersion && result.Record == query.Record && result.TraceVersion == query.TraceVersion && query.ByteOffset == 0 && strings.Contains(result.Content, "ASSET_TRACE:"+nonce) {
				detailed = true
			}
			continue
		}
		if call.Name != "read_graph" && call.Name != "read_snapshot" {
			continue
		}
		var query worker.GraphRequest
		var page struct {
			Section, StateVersion string
			Revision              int64
			Total, Offset         int
			NextOffset            *int `json:"next_offset"`
			Items                 []json.RawMessage
		}
		if json.Unmarshal(call.Input, &query) != nil {
			continue
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal(call.Output, &raw) != nil {
			continue
		}
		_ = json.Unmarshal(raw["section"], &page.Section)
		_ = json.Unmarshal(raw["state_version"], &page.StateVersion)
		_ = json.Unmarshal(raw["revision"], &page.Revision)
		_ = json.Unmarshal(raw["total"], &page.Total)
		_ = json.Unmarshal(raw["offset"], &page.Offset)
		_ = json.Unmarshal(raw["next_offset"], &page.NextOffset)
		_ = json.Unmarshal(raw["items"], &page.Items)
		if page.Section != query.Section {
			continue
		}
		if call.Name == "read_graph" && query.Section == "facts" && slices.Equal(query.AssetIDs, []string{want[2].ID}) && call.Index < publication {
			for _, raw := range page.Items {
				var fact board.FactRecord
				if json.Unmarshal(raw, &fact) == nil && fact.ID == producer.ID && fact.RunID == producer.RunID {
					filtered = true
				}
			}
		}
		if query.Section == "assets" {
			var assets []board.Asset
			for _, raw := range page.Items {
				var asset board.Asset
				if json.Unmarshal(raw, &asset) == nil {
					assets = append(assets, asset)
				}
			}
			if call.Name == "read_graph" && call.Index > publication && page.Total == 4 && slices.Equal(assetIDs(assets), assetIDs(want)) {
				liveAssets = true
			}
			if call.Name == "read_graph" && call.Index < publication && slices.Contains(assets, want[2]) {
				discovered = true
			}
			if call.Name == "read_snapshot" && query.Limit == 1 && len(assets) == 1 && page.Offset == query.Offset && page.Total == 3 && page.StateVersion == job.InputSnapshot.StateVersion && page.Revision == job.InputSnapshot.Revision {
				bound := query.Offset == 0 && call.Index < publication || query.Offset > 0 && call.Index > publication && query.ExpectedVersion == job.InputSnapshot.StateVersion
				if bound && (query.Offset < 2 && page.NextOffset != nil && *page.NextOffset == query.Offset+1 || query.Offset == 2 && page.NextOffset == nil) {
					frozen[query.Offset] = assets[0]
				}
			}
		}
		if query.Section == "anchors" && call.Index > publication {
			var anchors []board.AssetAnchor
			for _, raw := range page.Items {
				var anchor board.AssetAnchor
				if json.Unmarshal(raw, &anchor) == nil {
					anchors = append(anchors, anchor)
				}
			}
			if page.NextOffset != nil {
				continue
			}
			if call.Name == "read_snapshot" && page.StateVersion == job.InputSnapshot.StateVersion && page.Revision == job.InputSnapshot.Revision && query.ExpectedVersion == job.InputSnapshot.StateVersion {
				frozenAnchors = page.Total == 9 && len(anchors) == 9
				for _, asset := range want[:3] {
					for _, node := range []struct{ kind, id string }{{"step", producer.SourceStepID}, {"step", consumer.SourceStepID}, {"fact", producer.ID}} {
						frozenAnchors = frozenAnchors && slices.Contains(anchors, board.AssetAnchor{AssetID: asset.ID, NodeKind: node.kind, NodeID: node.id})
					}
				}
			}
			if call.Name == "read_graph" {
				var ids []string
				for _, a := range anchors {
					if a.NodeKind == "fact" && a.NodeID == consumer.ID {
						ids = append(ids, a.AssetID)
					}
				}
				slices.Sort(ids)
				liveAnchors = slices.Equal(ids, assetIDs(want))
			}
		}
	}
	check(discovered && filtered, "consumer did not discover the normalized asset and retrieve its producer Fact")
	check(listed && searched && detailed, "consumer lacks complete cross-Worker list/search/detail receipts for the original trace observation")
	var frozenList []board.Asset
	for _, asset := range frozen {
		frozenList = append(frozenList, asset)
	}
	check(len(frozen) == 3 && slices.Equal(assetIDs(frozenList), assetIDs(want[:3])), "snapshot pages did not retain their original three identities across publication")
	check(frozenAnchors && liveAssets && liveAnchors, "frozen and live catalog/anchor reads did not distinguish the new publication")
	return failures
}

// A fully matching synthetic journal is necessary to check the validator's
// negative cases offline; it is not used by the opt-in real-model workload.
func assetGraphDeliveryFixture(t *testing.T) (board.State, map[string][]byte) {
	t.Helper()
	state, files := curationRelationsDeliveryFixture(t)
	state.Curation = board.CurationProgress{}
	state.FactRelations = nil
	state.Assets = assetFixtureIdentities()
	state.AssetAnchors = nil
	state.FactRecords = nil
	state.Steps = nil
	const observed = "2026-10-08T00:00:00Z"
	const nonce = "0123456789abcdef0123456789abcdef"
	base := "/workspace/.pwnmesh/runs/producer-run/graph-tools/asset-seed/"
	seedRaw := []byte(`{"nonce":"` + nonce + `","values":[2,3,5]}` + "\n")
	receiptRaw, _ := json.Marshal(assetCalculation{Nonce: nonce, Sum: 10, SHA256: fmt.Sprintf("%x", sha256.Sum256(seedRaw))})
	files[assetConsumerPath] = receiptRaw
	at := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	cp := workergraph.Checkpoint{SchemaVersion: 1, RunID: "producer-run", Status: "succeeded"}
	for i, id := range []string{"seed", "receipt"} {
		stdout, stderr := base+"nodes/"+id+"/stdout.log", base+"nodes/"+id+"/stderr.log"
		files[stdout] = seedRaw
		if i == 1 {
			files[stdout] = receiptRaw
		}
		files[stderr] = []byte{}
		value, _ := json.Marshal(map[string]any{"output_path": stdout, "stderr_path": stderr, "exit_code": 0})
		cp.Nodes = append(cp.Nodes, workergraph.NodeState{ID: id, Kind: "function", Status: "succeeded", Attempt: 1, StartedAt: at.Add(time.Duration(i) * time.Second), FinishedAt: at.Add(time.Duration(i+1) * time.Second), Output: workergraph.Output{Value: value, Artifacts: []workergraph.Artifact{{Path: stdout, SHA256: fmt.Sprintf("%x", sha256.Sum256(files[stdout]))}, {Path: stderr, SHA256: fmt.Sprintf("%x", sha256.Sum256(files[stderr]))}}}})
	}
	files[base+"graph.json"], _ = json.Marshal(cp)
	files[base+"nodes/receipt/dependencies.json"], _ = json.Marshal(cp.Nodes[:1])
	appendCall := func(run, name string, input, output any) {
		path := "/workspace/.pwnmesh/runs/" + run + "/events.jsonl"
		id := fmt.Sprintf("call-%d", len(files[path]))
		in, _ := json.Marshal(input)
		out, _ := json.Marshal(output)
		if value, ok := output.(string); ok {
			out = []byte(value)
		}
		text, _ := json.Marshal(string(out))
		for _, message := range []agent.Message{{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: id, Name: name, Input: in}}}, {Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: id, Content: text}}}} {
			raw, _ := json.Marshal(agent.Event{Type: "message", Message: &message})
			files[path] = append(files[path], append(raw, '\n')...)
		}
	}
	files["/workspace/.pwnmesh/runs/producer-run/events.jsonl"] = nil
	appendCall("producer-run", "run_graph", map[string]any{"key": "asset-seed", "nodes": []map[string]any{{"id": "seed", "command": "python generate"}, {"id": "receipt", "command": "python compute", "depends_on": []workergraph.Dependency{{ID: "seed"}}}}}, map[string]any{"key": "asset-seed", "status": "succeeded", "nodes": cp.Nodes})
	appendCall("producer-run", "bash", map[string]string{"command": "print marker"}, "ASSET_TRACE:"+nonce)
	for _, name := range []string{"producer", "consumer"} {
		id, run, workerID := name+"-fact", name+"-run", "controlled@"+name+"-run"
		fact := board.FactRecord{ID: id, Scope: assetProducerScope, Description: "Local computation retained", ObservedAt: observed, Status: "valid", RunID: workerID, SourceStepID: name, Evidence: []board.EvidenceRef{retainOrchestrationEvidence(run, seedRaw, files), retainOrchestrationEvidence(run, receiptRaw, files)}}
		if name == "consumer" {
			fact.Scope = assetConsumerScope
		}
		state.FactRecords = append(state.FactRecords, fact)
		step := board.Step{ID: name, Status: "completed", SupportValid: true, Worker: &workerID, Result: &id}
		if name == "consumer" {
			step.From = []string{"producer-fact"}
		}
		state.Steps = append(state.Steps, step)
		for _, asset := range state.Assets[:3] {
			state.AssetAnchors = append(state.AssetAnchors, board.AssetAnchor{AssetID: asset.ID, NodeKind: "step", NodeID: name})
		}
		anchors := state.Assets[:3]
		if name == "consumer" {
			anchors = state.Assets
		}
		for _, asset := range anchors {
			state.AssetAnchors = append(state.AssetAnchors, board.AssetAnchor{AssetID: asset.ID, NodeKind: "fact", NodeID: id})
		}
		job := worker.Job{RunID: run, Kind: "explore", GraphRPC: true, ResultContractVersion: 2, Graph: state.Graph, Intent: &board.Intent{ID: name}, InputSnapshot: &board.InputSnapshot{Version: 1, ID: "snapshot", StateVersion: "frozen-version", Revision: 3}}
		files["/workspace/.pwnmesh/runs/"+run+"/job.json"], _ = json.Marshal(job)
		files["/workspace/.pwnmesh/runs/"+run+"/session.json"], _ = json.Marshal(map[string]any{"run_id": run, "identity": map[string]string{"project_id": state.Graph.Project.ID, "step_id": name, "run_id": run}, "result": map[string]any{"status": "success", "text": `{"accepted":true,"outcome":"completed","data":{"fact_id":"` + id + `"}}`}})
	}
	state.Goals = []board.Goal{{ID: "goal", Status: "achieved", SupportValid: true, Sources: []string{"consumer-fact"}}}
	page := func(section string, items any, total, offset int, next *int, frozen bool) map[string]any {
		version, rev := "live-version", 5
		if frozen {
			version, rev = "frozen-version", 3
		}
		return map[string]any{"section": section, "items": items, "total": total, "offset": offset, "next_offset": next, "state_version": version, "revision": rev}
	}
	next := 1
	appendCall("consumer-run", "read_snapshot", map[string]any{"section": "assets", "limit": 1}, page("assets", state.Assets[:1], 3, 0, &next, true))
	appendCall("consumer-run", "read_graph", map[string]any{"section": "assets"}, page("assets", state.Assets[:3], 3, 0, nil, false))
	appendCall("consumer-run", "read_graph", map[string]any{"section": "facts", "asset_ids": []string{state.Assets[2].ID}}, page("facts", state.FactRecords[:1], 1, 0, nil, false))
	appendCall("consumer-run", "read_worker_trace", map[string]any{}, map[string]any{"items": []map[string]string{{"run_id": "producer-run", "step_id": "producer"}}})
	appendCall("consumer-run", "read_worker_trace", map[string]string{"run_id": "producer-run", "query": "ASSET_TRACE:"}, map[string]any{"run_id": "producer-run", "step_id": "producer", "trace_version": "trace-version", "projection": true, "items": []map[string]string{{"record": "r1", "tool": "bash", "content": "ASSET_TRACE:" + nonce}}})
	appendCall("consumer-run", "read_worker_trace", map[string]string{"run_id": "producer-run", "record": "r1", "trace_version": "trace-version"}, map[string]any{"run_id": "producer-run", "step_id": "producer", "trace_version": "trace-version", "projection": true, "record": "r1", "content": "ASSET_TRACE:" + nonce})
	// bash results are strings containing raw command output, not JSON objects.
	appendCall("consumer-run", "bash", map[string]string{"command": "calculate"}, string(receiptRaw))
	appendCall("consumer-run", "graph_action", board.StateAction{Op: "fact", Payload: json.RawMessage(`{}`)}, board.StateActionResult{Op: "fact", ID: "consumer-fact", Revision: 5, StateVersion: "live-version"})
	for offset := 1; offset < 3; offset++ {
		var next *int
		if offset == 1 {
			n := 2
			next = &n
		}
		appendCall("consumer-run", "read_snapshot", map[string]any{"section": "assets", "offset": offset, "limit": 1, "expected_version": "frozen-version"}, page("assets", state.Assets[offset:offset+1], 3, offset, next, true))
	}
	var frozenAnchors []board.AssetAnchor
	for _, a := range state.AssetAnchors {
		if a.NodeID != "consumer-fact" {
			frozenAnchors = append(frozenAnchors, a)
		}
	}
	appendCall("consumer-run", "read_snapshot", map[string]string{"section": "anchors", "expected_version": "frozen-version"}, page("anchors", frozenAnchors, len(frozenAnchors), 0, nil, true))
	appendCall("consumer-run", "read_graph", map[string]string{"section": "assets"}, page("assets", state.Assets, 4, 0, nil, false))
	appendCall("consumer-run", "read_graph", map[string]string{"section": "anchors"}, page("anchors", state.AssetAnchors, len(state.AssetAnchors), 0, nil, false))
	return state, files
}

func TestAssetGraphAcceptanceRejectsMissingRuntimeProof(t *testing.T) {
	state, files := assetGraphDeliveryFixture(t)
	if failures := validateAssetGraphDelivery(state, files); len(failures) > 0 {
		t.Fatal(failures)
	}
	for _, tc := range []struct {
		name   string
		change func(*board.State, map[string][]byte)
	}{
		{"missing_asset", func(s *board.State, _ map[string][]byte) { s.Assets = s.Assets[:3] }},
		{"missing_anchor", func(s *board.State, _ map[string][]byte) { s.AssetAnchors = s.AssetAnchors[1:] }},
		{"same_worker", func(s *board.State, _ map[string][]byte) { s.FactRecords[1].RunID = s.FactRecords[0].RunID }},
		{"missing_consumer_journal", func(_ *board.State, f map[string][]byte) {
			delete(f, "/workspace/.pwnmesh/runs/consumer-run/events.jsonl")
		}},
		{"failed_trace_result", func(_ *board.State, f map[string][]byte) {
			p := "/workspace/.pwnmesh/runs/consumer-run/events.jsonl"
			f[p] = bytes.ReplaceAll(f[p], []byte(`"type":"tool_result"`), []byte(`"type":"tool_result","is_error":true`))
		}},
		{"trace_wrong_run", func(_ *board.State, f map[string][]byte) {
			p := "/workspace/.pwnmesh/runs/consumer-run/events.jsonl"
			f[p] = bytes.ReplaceAll(f[p], []byte("producer-run"), []byte("different-run"))
		}},
		{"snapshot_mixed_version", func(_ *board.State, f map[string][]byte) {
			p := "/workspace/.pwnmesh/runs/consumer-run/events.jsonl"
			f[p] = bytes.ReplaceAll(f[p], []byte("frozen-version"), []byte("new-version"))
		}},
		{"no_dag_journal", func(_ *board.State, f map[string][]byte) {
			delete(f, "/workspace/.pwnmesh/runs/producer-run/events.jsonl")
		}},
		{"tampered_dag_bytes", func(_ *board.State, f map[string][]byte) {
			f["/workspace/.pwnmesh/runs/producer-run/graph-tools/asset-seed/nodes/seed/stdout.log"] = []byte(`{}`)
		}},
		{"forged_dependencies", func(_ *board.State, f map[string][]byte) {
			f["/workspace/.pwnmesh/runs/producer-run/graph-tools/asset-seed/nodes/receipt/dependencies.json"] = []byte(`[]`)
		}},
		{"unsupported_completion", func(s *board.State, _ map[string][]byte) { s.Goals[0].Sources = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := assetGraphDeliveryFixture(t)
			tc.change(&s, f)
			if len(validateAssetGraphDelivery(s, f)) == 0 {
				t.Fatal("accepted asset delivery without its runtime proof")
			}
		})
	}
}
