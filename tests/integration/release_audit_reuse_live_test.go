//go:build linux

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/workergraph"
)

func TestLiveReleaseAuditReuseProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_RELEASE_AUDIT_REUSE_TEST") != "1" {
		t.Skip("opt in to real model verified graph reuse after a deterministic join failure")
	}
	fixture, observations := newReleaseAuditReuseHTTP(t)
	origin := fmt.Sprintf(`Audit synthetic Android and Linux client releases at %s using two independent platform Steps and a separate delivery Step whose structured depends_on names both successful platform Steps. Only this local HTTP origin and /workspace are authorized; do not contact manifest endpoints, read credentials, install software, or execute downloaded files. Python stdlib is sufficient. These are inert ZIP fixtures, not runnable APKs or binaries. Preserve this acceptance contract in delegated tasks.
For each platform, retain original /catalog/{platform}.json at /workspace/release-audit/{platform}/source.json, attempt each catalog package path exactly once, and retain original successful bytes at /workspace/release-audit/{platform}/downloads/{id}.zip. Missing files are findings, not failed download nodes. Catalog entries have id/product/version/path/sha256, ZIP manifest.json has product/version/debug/endpoints. Inspect all available packages even if their hash mismatches. Do not fetch any catalog or package a second time.
Each platform must first submit one complete mixed run_graph with key release-{platform} and exactly these IDs: download (command), manifest (child Agent), integrity (command), join (command). download declares artifacts ["catalog.json","acquisition.json"] in its private node directory, in addition to retained workspace copies; acquisition.json records every HTTP status. manifest and integrity both depend on download, explicitly list inputs [{"node":"download","artifact":"catalog.json"},{"node":"download","artifact":"acquisition.json"}], and run concurrently with resources:[]. manifest inspects embedded versions/debug/endpoints and declares manifests.json; integrity computes hashes, missing packages and duplicate catalog product+version, declaring integrity.json. Shared original downloads are read-only. Do not simulate the child Agent in shell. Child task text must contain this context, exact file contracts and checks.
join depends on all three predecessors and explicitly lists the four required input files using inputs:[{node,artifact}]. PWNMESH_DEPENDENCIES is the path of a JSON file containing node receipts; look up each declared dependency file through dependency.output.files[artifact_name]. Do not treat stdout logs or the artifacts array as positional business data. All four original nodes use resources:[], each command may print human diagnostics to stdout, and machine-readable data must be declared files. Keep normal default graph parallelism.
For this acceptance scenario, the FIRST join must GET %s/fault/{platform}/join exactly once before producing a report; this endpoint intentionally returns HTTP 503 once. On 503 immediately exit nonzero. Do not catch-and-retry, mark optional, bypass this fault, or run the join outside run_graph. The download, manifest and integrity nodes must already have succeeded, so the first graph fails only at join. Inspect the failed receipt/log, then recover with one NEW key release-{platform}-recovered. Under that key, import exactly the three successful source nodes as IDs download, manifest, integrity with kind:"reuse", reuse_from:{key:"release-{platform}",node:"source ID"}, resources:[], and no command/task/artifacts/depends_on/inputs overrides. These imports verify and reuse their original files; do not copy or rerun their work and do not create another child Agent. Add a new command join depending on all imports with the same four declared input bindings. That join GETs the fault endpoint exactly once (now 200) then computes the report. Do not modify old graph definitions, checkpoint, session, events or source outputs. Do not suppress or erase the first failed graph.
The successful join writes /workspace/release-audit/{platform}/report.json plus an identical declared report.json in its private node directory, with exact schema {"platform":"android or linux","manifest_sha256":"SHA256 of original catalog bytes","entries":[{"id":"catalog id","http_status":200 or 404,"sha256":"actual package SHA256 or empty if missing","embedded_version":"ZIP version or empty if missing","issues":[codes]}]}. Each entry appears once. Codes: missing for 404; hash_mismatch for actual versus catalog SHA; version_mismatch for ZIP versus catalog version; duplicate_release on EVERY row with duplicate catalog product+version; debug_enabled for debug true; cleartext_endpoint for any http:// endpoint. Missing rows have only missing; clean rows have an empty issues array. Compute from retained bytes, do not guess. Each platform finishes an evidence-backed fact scoped exactly release-audit/android or release-audit/linux with original source.json and joined report.json evidence.
After both platform Steps succeed, delivery reads bound results, verifies reports against retained originals, and writes /workspace/release-audit/report.json as {"platforms":[both complete platform reports],"release_ready":boolean,"issue_count":total issue codes across all entries}. Ready means zero issues. Finish scope release-audit/delivery citing the final JSON. Preserve all original downloads, branch outputs, both failed graphs and both verified recovery graphs. Do not repair the intentionally defective fixtures.`, fixture.URL, fixture.URL)
	runObservedProject(t, "Release audit with failed joins and verified successful-node reuse", origin,
		"Download Android and Linux release packages once, inspect them with parallel command/Agent branches, recover a controlled join failure by verified reuse without repeating acquisition or child analysis, and deliver the complete reproducible report.",
		"release_audit_verified_reuse_after_join_failure_v1", func(state board.State, files map[string][]byte) []string {
			requests := observations.snapshot()
			failures := append(validateReleaseAuditDeliveryWithGraphs(state, files, validateReleaseAuditReuseGraph), validateReleaseAuditReuseHTTP(requests)...)
			if err := saveLiveJSON(filepath.Join(os.Getenv("PWNMESH_LIVE_OUTPUT"), "release-http-requests.json"), requests); err != nil {
				failures = append(failures, "cannot retain fixture HTTP observations: "+err.Error())
			}
			return failures
		}, 1)
}

func TestRetainedReleaseAuditReuseProject(t *testing.T) {
	dir := os.Getenv("PWNMESH_RELEASE_AUDIT_REUSE_REPLAY_DIR")
	if dir == "" {
		t.Skip("opt in with retained state.json, workspace.tar and release-http-requests.json")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state board.State
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	archive, err := os.Open(filepath.Join(dir, "workspace.tar"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	files, err := retainLiveWorkspace(archive, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	requestRaw, err := os.ReadFile(filepath.Join(dir, "release-http-requests.json"))
	if err != nil {
		t.Fatal(err)
	}
	var requests map[string]int
	if err = json.Unmarshal(requestRaw, &requests); err != nil {
		t.Fatal(err)
	}
	failures := append(validateReleaseAuditDeliveryWithGraphs(state, files, validateReleaseAuditReuseGraph), validateReleaseAuditReuseHTTP(requests)...)
	if len(failures) != 0 {
		t.Fatal(failures)
	}
}

type releaseAuditReuseInput struct {
	Node     string `json:"node"`
	Artifact string `json:"artifact"`
}

type releaseAuditReuseSource struct {
	Key, Node string
}

type releaseAuditReuseNode struct {
	ID, Kind  string
	DependsOn []workergraph.Dependency `json:"depends_on"`
	Inputs    []releaseAuditReuseInput
	Artifacts []string
	ReuseFrom *releaseAuditReuseSource `json:"reuse_from"`
}

type releaseAuditReuseCall struct {
	Key   string
	Nodes []releaseAuditReuseNode
}

type releaseAuditReuseObservation struct {
	Call      releaseAuditReuseCall
	Receipt   workergraph.Checkpoint
	Files     map[string]map[string]workergraph.Artifact
	Submitted int
	Returned  int
}

func releaseAuditReuseObservations(raw []byte) map[string][]releaseAuditReuseObservation {
	result := map[string][]releaseAuditReuseObservation{}
	pending := map[string]releaseAuditReuseObservation{}
	for index, line := range bytes.Split(raw, []byte{'\n'}) {
		var event agent.Event
		if json.Unmarshal(line, &event) != nil || event.Message == nil {
			continue
		}
		for _, block := range event.Message.Content {
			if event.Message.Role == "assistant" && block.Type == "tool_use" && block.Name == "run_graph" {
				var call releaseAuditReuseCall
				if json.Unmarshal(block.Input, &call) == nil {
					pending[block.ID] = releaseAuditReuseObservation{Call: call, Submitted: index}
				}
			}
			observation, exists := pending[block.ToolUseID]
			if event.Message.Role != "user" || block.Type != "tool_result" || !exists {
				continue
			}
			var text string
			var receipt struct {
				Key, Status string
				Nodes       []workergraph.NodeState
			}
			if json.Unmarshal(block.Content, &text) != nil || json.Unmarshal([]byte(text), &receipt) != nil || receipt.Key != observation.Call.Key {
				continue
			}
			observation.Returned = index
			observation.Receipt.Status, observation.Receipt.Nodes = receipt.Status, receipt.Nodes
			observation.Files = releaseAuditReuseViewFiles([]byte(text), true)
			result[receipt.Key] = append(result[receipt.Key], observation)
			delete(pending, block.ToolUseID)
		}
	}
	return result
}

func releaseAuditReuseViewFiles(raw []byte, receipt bool) map[string]map[string]workergraph.Artifact {
	type view struct {
		ID     string
		Output struct {
			Files map[string]workergraph.Artifact
		}
	}
	var nodes []view
	if receipt {
		var wrapper struct{ Nodes []view }
		_ = json.Unmarshal(raw, &wrapper)
		nodes = wrapper.Nodes
	} else {
		_ = json.Unmarshal(raw, &nodes)
	}
	result := map[string]map[string]workergraph.Artifact{}
	for _, node := range nodes {
		result[node.ID] = node.Output.Files
	}
	return result
}

func releaseAuditReuseOutputMatches(source, reused workergraph.NodeState, key string) bool {
	var original, imported map[string]any
	a, _ := json.Marshal(source.Output)
	b, _ := json.Marshal(reused.Output)
	if json.Unmarshal(a, &original) != nil || json.Unmarshal(b, &imported) != nil {
		return false
	}
	value, ok := imported["value"].(map[string]any)
	if !ok {
		return false
	}
	provenance, ok := value["reused_from"].(map[string]any)
	if !ok || provenance["key"] != key || provenance["node"] != source.ID || provenance["kind"] != source.Kind || provenance["definition_sha256"] != source.DefinitionSHA256 || provenance["input_sha256"] != source.InputSHA256 {
		return false
	}
	delete(value, "reused_from")
	return reused.Kind == source.Kind && reflect.DeepEqual(original, imported)
}

func validateReleaseAuditReuseGraph(run, platform string, files map[string][]byte) []string {
	var failures []string
	check := func(ok bool, reason string) {
		if !ok {
			failures = append(failures, platform+": "+reason)
		}
	}
	runBase := "/workspace/.pwnmesh/runs/" + run + "/"
	originalKey, recoveredKey := "release-"+platform, "release-"+platform+"-recovered"
	originalBase, recoveredBase := runBase+"graph-tools/"+originalKey+"/", runBase+"graph-tools/"+recoveredKey+"/"
	var original, recovered workergraph.Checkpoint
	if json.Unmarshal(files[originalBase+"graph.json"], &original) != nil || json.Unmarshal(files[recoveredBase+"graph.json"], &recovered) != nil {
		return []string{platform + ": missing original or recovered graph checkpoint"}
	}
	check(original.RunID == run && original.Status == "failed" && len(original.Nodes) == 4, "original failed graph identity or node count changed")
	check(recovered.RunID == run && recovered.Status == "succeeded" && len(recovered.Nodes) == 4, "recovered graph identity/status/node count mismatch")
	observations := releaseAuditReuseObservations(files[runBase+"events.jsonl"])
	if len(observations[originalKey]) != 1 || len(observations[recoveredKey]) != 1 || len(observations) != 2 {
		return append(failures, platform+": expected exactly one original and one recovered graph execution receipt")
	}
	initial, repair := observations[originalKey][0], observations[recoveredKey][0]
	check(initial.Returned < repair.Submitted, "recovery was submitted before observing the first failure")
	check(initial.Receipt.Status == original.Status && reflect.DeepEqual(initial.Receipt.Nodes, original.Nodes), "source checkpoint differs from its original parent receipt")
	check(repair.Receipt.Status == recovered.Status && reflect.DeepEqual(repair.Receipt.Nodes, recovered.Nodes), "recovery checkpoint differs from parent receipt")
	oldNodes, newNodes := map[string]workergraph.NodeState{}, map[string]workergraph.NodeState{}
	oldSpecs, newSpecs := map[string]releaseAuditReuseNode{}, map[string]releaseAuditReuseNode{}
	for _, node := range original.Nodes {
		oldNodes[node.ID] = node
	}
	for _, node := range recovered.Nodes {
		newNodes[node.ID] = node
	}
	for _, spec := range initial.Call.Nodes {
		oldSpecs[spec.ID] = spec
	}
	for _, spec := range repair.Call.Nodes {
		newSpecs[spec.ID] = spec
	}
	check(len(oldSpecs) == 4 && len(newSpecs) == 4, "submitted graph does not contain four distinct nodes")
	for _, id := range []string{"download", "manifest", "integrity"} {
		source, imported := oldNodes[id], newNodes[id]
		check(source.Status == "succeeded" && source.Attempt == 1 && source.Error == "" && len(source.DefinitionSHA256) == 64 && len(source.InputSHA256) == 64 && !source.StartedAt.IsZero() && source.FinishedAt.After(source.StartedAt), "source was not successfully executed once: "+id)
		check(imported.Status == "succeeded" && imported.Attempt == 1 && imported.Error == "" && imported.StartedAt.After(oldNodes["join"].FinishedAt), "import happened before the original failure or did not succeed: "+id)
		spec := newSpecs[id]
		check(spec.Kind == "reuse" && spec.ReuseFrom != nil && *spec.ReuseFrom == (releaseAuditReuseSource{Key: originalKey, Node: id}), "recovered node did not explicitly import its successful source: "+id)
		check(releaseAuditReuseOutputMatches(source, imported, originalKey), "import changed original outputs or omitted verified provenance: "+id)
		for _, artifact := range source.Output.Artifacts {
			body, exists := files[artifact.Path]
			check(exists && fmt.Sprintf("%x", sha256.Sum256(body)) == artifact.SHA256, "source artifact changed: "+artifact.Path)
		}
		declared := initial.Files[id]
		check(len(declared) == len(oldSpecs[id].Artifacts), "named files differ from declared source artifacts: "+id)
		check(reflect.DeepEqual(declared, repair.Files[id]), "import changed the directly addressable files: "+id)
		for _, name := range oldSpecs[id].Artifacts {
			artifact, exists := declared[name]
			check(exists && artifact.Path == originalBase+"nodes/"+id+"/"+name && slices.Contains(source.Output.Artifacts, artifact), "source declared output is not directly addressable: "+id+"/"+name)
		}
	}
	check(oldSpecs["download"].Kind != "agent" && oldSpecs["manifest"].Kind == "agent" && oldSpecs["integrity"].Kind != "agent", "source graph lost mixed Agent/command execution")
	check(oldNodes["join"].Status == "failed" && oldNodes["join"].Attempt == 1 && oldNodes["join"].Error != "", "original join did not fail exactly once")
	check(newSpecs["join"].Kind != "agent" && newSpecs["join"].Kind != "reuse" && newNodes["join"].Status == "succeeded", "recovery did not execute only a new command join")
	check(oldNodes["manifest"].StartedAt.Before(oldNodes["integrity"].FinishedAt) && oldNodes["integrity"].StartedAt.Before(oldNodes["manifest"].FinishedAt), "source Agent and integrity command did not overlap")
	for _, graph := range []struct {
		base  string
		nodes map[string]workergraph.NodeState
		specs map[string]releaseAuditReuseNode
	}{{originalBase, oldNodes, oldSpecs}, {recoveredBase, newNodes, newSpecs}} {
		for _, id := range []string{"manifest", "integrity", "join"} {
			spec := graph.specs[id]
			if spec.Kind == "reuse" {
				continue
			}
			var dependencies []workergraph.NodeState
			dependencyRaw := files[graph.base+"nodes/"+id+"/dependencies.json"]
			check(json.Unmarshal(dependencyRaw, &dependencies) == nil && len(dependencies) == len(spec.DependsOn), "missing frozen dependencies: "+graph.base+id)
			dependencyFiles := releaseAuditReuseViewFiles(dependencyRaw, false)
			for _, dep := range spec.DependsOn {
				producer, exists := graph.nodes[dep.ID]
				check(exists && !dep.Optional && !graph.nodes[id].StartedAt.Before(producer.FinishedAt), "consumer started before successful required source: "+id)
				check(slices.ContainsFunc(dependencies, func(saved workergraph.NodeState) bool { return reflect.DeepEqual(saved, producer) }), "consumer dependency does not match producer: "+id)
			}
			wanted := []releaseAuditReuseInput{{"download", "catalog.json"}, {"download", "acquisition.json"}}
			if id == "join" {
				wanted = append(wanted, releaseAuditReuseInput{"manifest", "manifests.json"}, releaseAuditReuseInput{"integrity", "integrity.json"})
			}
			check(len(spec.Inputs) == len(wanted), "consumer omitted explicit file input contract: "+id)
			for _, input := range wanted {
				check(slices.Contains(spec.Inputs, input) && slices.ContainsFunc(spec.DependsOn, func(dep workergraph.Dependency) bool { return dep.ID == input.Node && !dep.Optional }), "consumer omitted a required named file: "+id+"/"+input.Artifact)
				file, exists := dependencyFiles[input.Node][input.Artifact]
				check(exists && slices.Contains(graph.nodes[input.Node].Output.Artifacts, file), "consumer view omitted its named dependency file: "+id+"/"+input.Artifact)
			}
		}
	}
	joined, exists := repair.Files["join"]["report.json"]
	check(exists && joined.Path == recoveredBase+"nodes/join/report.json" && bytes.Equal(files[joined.Path], files[releaseAuditRoot+platform+"/report.json"]) && fmt.Sprintf("%x", sha256.Sum256(files[joined.Path])) == joined.SHA256, "recovered join report is not bound to its declared file")
	childBase := originalBase + "nodes/manifest/"
	var child struct {
		RunID, GraphKey, NodeID string
		History                 []agent.Message
		Result, Error           string
		Log                     struct {
			Offset int64
			SHA256 string
		} `json:"log_checkpoint"`
	}
	// Explicit JSON tags are needed for the child identity's snake_case fields.
	var identity struct {
		RunID    string `json:"run_id"`
		GraphKey string `json:"graph_key"`
		NodeID   string `json:"node_id"`
	}
	childRaw, childEvents := files[childBase+"session.json"], files[childBase+"events.jsonl"]
	_ = json.Unmarshal(childRaw, &identity)
	check(json.Unmarshal(childRaw, &child) == nil && identity.RunID == run && identity.GraphKey == originalKey && identity.NodeID == "manifest" && child.Result != "" && child.Error == "", "source child session is missing or unsuccessful")
	check(len(childEvents) > 0 && child.Log.Offset == int64(len(childEvents)) && child.Log.SHA256 == fmt.Sprintf("%x", sha256.Sum256(childEvents)), "source child journal does not match its persisted session")
	toolUsed, timedEvent := false, false
	for _, message := range child.History {
		for _, block := range message.Content {
			toolUsed = toolUsed || message.Role == "user" && block.Type == "tool_result" && !block.IsError
		}
	}
	for _, line := range bytes.Split(childEvents, []byte{'\n'}) {
		var event agent.Event
		if json.Unmarshal(line, &event) == nil && event.At != "" {
			at, err := time.Parse(time.RFC3339Nano, event.At)
			check(err == nil && !at.After(oldNodes["join"].StartedAt), "child event occurred after the first join started")
			timedEvent = timedEvent || err == nil
		}
	}
	check(toolUsed && timedEvent, "missing independently observed child tools and event timing")
	sessions, journals := 0, 0
	for path := range files {
		if !strings.HasPrefix(path, runBase+"graph-tools/") || !strings.Contains(path, "/nodes/") {
			continue
		}
		if strings.HasSuffix(path, "/session.json") {
			sessions++
			check(path == childBase+"session.json", "extra child session indicates repeated analysis: "+path)
		}
		if strings.HasSuffix(path, "/events.jsonl") {
			journals++
			check(path == childBase+"events.jsonl", "extra child journal indicates repeated analysis: "+path)
		}
	}
	check(sessions == 1 && journals == 1, "expected exactly one actual child session and journal per platform")
	return failures
}

// The gate fails the first join request after both expensive branches have
// completed. HTTP counts are independent observations of acquisition and retry;
// they do not trust the model's explanation of what it executed.
type releaseAuditReuseHTTP struct {
	mu       sync.Mutex
	requests map[string]int
	gates    map[string]int
}

func newReleaseAuditReuseHTTP(t *testing.T) (*httptest.Server, *releaseAuditReuseHTTP) {
	t.Helper()
	files, _ := releaseAuditFixture()
	observations := &releaseAuditReuseHTTP{requests: map[string]int{}, gates: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, exists := files[r.URL.Path]
		status := http.StatusOK
		observations.mu.Lock()
		if r.Method != http.MethodGet {
			status = http.StatusNotFound
		} else if slices.Contains([]string{"/fault/android/join", "/fault/linux/join"}, r.URL.Path) {
			observations.gates[r.URL.Path]++
			body = []byte(`{"join_ready":true}`)
			if observations.gates[r.URL.Path] == 1 {
				status = http.StatusServiceUnavailable
				body = []byte(`{"error":"intentional first join failure; recover using verified successful nodes"}`)
			}
		} else if !exists {
			status = http.StatusNotFound
		}
		observations.requests[fmt.Sprintf("%s %s %d", r.Method, r.URL.Path, status)]++
		observations.mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server, observations
}

func (observations *releaseAuditReuseHTTP) snapshot() map[string]int {
	observations.mu.Lock()
	defer observations.mu.Unlock()
	result := make(map[string]int, len(observations.requests))
	for key, count := range observations.requests {
		result[key] = count
	}
	return result
}

func validateReleaseAuditReuseHTTP(requests map[string]int) []string {
	files, expected := releaseAuditFixture()
	wanted := map[string]int{}
	for path := range files {
		wanted["GET "+path+" 200"] = 1
	}
	for _, platform := range []string{"android", "linux"} {
		for _, entry := range expected[platform].Entries {
			if entry.HTTPStatus == http.StatusNotFound {
				wanted["GET /packages/"+entry.ID+".zip 404"] = 1
			}
		}
		wanted["GET /fault/"+platform+"/join 503"] = 1
		wanted["GET /fault/"+platform+"/join 200"] = 1
	}
	var failures []string
	for key, count := range wanted {
		if requests[key] != count {
			failures = append(failures, fmt.Sprintf("HTTP receipt %s: expected %d attempt, observed %d", key, count, requests[key]))
		}
	}
	for key, count := range requests {
		if _, allowed := wanted[key]; !allowed && count != 0 {
			failures = append(failures, "unexpected fixture request: "+key)
		}
	}
	slices.Sort(failures)
	return failures
}

func releaseAuditReuseHTTPFixture(t *testing.T) map[string]int {
	t.Helper()
	server, observations := newReleaseAuditReuseHTTP(t)
	files, expected := releaseAuditFixture()
	request := func(path string, status int) {
		t.Helper()
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if _, err = io.Copy(io.Discard, response.Body); err != nil || response.StatusCode != status {
			t.Fatalf("GET %s: status=%d, error=%v", path, response.StatusCode, err)
		}
	}
	for path := range files {
		request(path, http.StatusOK)
	}
	for _, platform := range []string{"android", "linux"} {
		for _, entry := range expected[platform].Entries {
			if entry.HTTPStatus == http.StatusNotFound {
				request("/packages/"+entry.ID+".zip", http.StatusNotFound)
			}
		}
		request("/fault/"+platform+"/join", http.StatusServiceUnavailable)
		request("/fault/"+platform+"/join", http.StatusOK)
	}
	return observations.snapshot()
}

func TestReleaseAuditReuseHTTPRequiresSingleAcquisitionAndOneFailedJoin(t *testing.T) {
	valid := releaseAuditReuseHTTPFixture(t)
	if failures := validateReleaseAuditReuseHTTP(valid); len(failures) != 0 {
		t.Fatal(failures)
	}
	for _, test := range []struct {
		name, key string
		count     int
	}{
		{"catalog downloaded twice", "GET /catalog/android.json 200", 2},
		{"package downloaded twice", "GET /packages/linux-0.zip 200", 2},
		{"missing package not attempted", "GET /packages/android-3.zip 404", 0},
		{"failure bypassed", "GET /fault/android/join 503", 0},
		{"join requested three times", "GET /fault/linux/join 200", 2},
		{"unexpected request", "GET /unexpected 404", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := make(map[string]int, len(valid))
			for key, count := range valid {
				changed[key] = count
			}
			changed[test.key] = test.count
			if failures := validateReleaseAuditReuseHTTP(changed); len(failures) == 0 || !strings.Contains(strings.Join(failures, "\n"), test.key) {
				t.Fatalf("missing independent HTTP violation: %v", failures)
			}
		})
	}
}

func releaseAuditReuseGraphFixture(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	save := func(path string, value any) {
		files[path], _ = json.Marshal(value)
	}
	runBase := "/workspace/.pwnmesh/runs/audit-run/"
	oldBase, newBase := runBase+"graph-tools/release-android/", runBase+"graph-tools/release-android-recovered/"
	inputs := []releaseAuditReuseInput{{"download", "catalog.json"}, {"download", "acquisition.json"}}
	joinInputs := append(append([]releaseAuditReuseInput{}, inputs...), releaseAuditReuseInput{"manifest", "manifests.json"}, releaseAuditReuseInput{"integrity", "integrity.json"})
	branches := []workergraph.Dependency{{ID: "download"}}
	joinDeps := []workergraph.Dependency{{ID: "download"}, {ID: "manifest"}, {ID: "integrity"}}
	oldCall := releaseAuditReuseCall{Key: "release-android", Nodes: []releaseAuditReuseNode{
		{ID: "download", Kind: "command", Artifacts: []string{"catalog.json", "acquisition.json"}},
		{ID: "manifest", Kind: "agent", DependsOn: branches, Inputs: inputs, Artifacts: []string{"manifests.json"}},
		{ID: "integrity", Kind: "command", DependsOn: branches, Inputs: inputs, Artifacts: []string{"integrity.json"}},
		{ID: "join", Kind: "command", DependsOn: joinDeps, Inputs: joinInputs, Artifacts: []string{"report.json"}},
	}}
	newCall := releaseAuditReuseCall{Key: "release-android-recovered"}
	oldCheckpoint := workergraph.Checkpoint{RunID: "audit-run", Status: "failed"}
	newCheckpoint := workergraph.Checkpoint{RunID: "audit-run", Status: "succeeded"}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	timings := [][2]time.Duration{{0, time.Second}, {time.Second, 3 * time.Second}, {time.Second, 2 * time.Second}, {3 * time.Second, 4 * time.Second}}
	for index, spec := range oldCall.Nodes {
		kind := "function"
		if spec.Kind == "agent" {
			kind = "agent"
		}
		node := workergraph.NodeState{ID: spec.ID, Kind: kind, Status: "succeeded", Attempt: 1, DefinitionSHA256: strings.Repeat("d", 64), InputSHA256: strings.Repeat("a", 64), StartedAt: start.Add(timings[index][0]), FinishedAt: start.Add(timings[index][1])}
		for _, name := range append([]string{"stdout.log"}, spec.Artifacts...) {
			if spec.ID == "join" && name != "stdout.log" {
				continue
			}
			path := oldBase + "nodes/" + spec.ID + "/" + name
			body := []byte(`{"source":"` + spec.ID + `","file":"` + name + `"}`)
			files[path] = body
			node.Output.Artifacts = append(node.Output.Artifacts, workergraph.Artifact{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(body))})
		}
		node.Output.Value, _ = json.Marshal(map[string]any{"stdout": "diagnostic", "output_path": oldBase + "nodes/" + spec.ID + "/stdout.log", "exit_code": 0})
		if spec.ID == "join" {
			node.Status, node.Error = "failed", "controlled join failure: HTTP 503"
		}
		oldCheckpoint.Nodes = append(oldCheckpoint.Nodes, node)
		if spec.ID != "join" {
			newCall.Nodes = append(newCall.Nodes, releaseAuditReuseNode{ID: spec.ID, Kind: "reuse", ReuseFrom: &releaseAuditReuseSource{Key: oldCall.Key, Node: spec.ID}})
			var value map[string]any
			_ = json.Unmarshal(node.Output.Value, &value)
			value["reused_from"] = map[string]string{"key": oldCall.Key, "node": spec.ID, "kind": kind, "definition_sha256": node.DefinitionSHA256, "input_sha256": node.InputSHA256}
			node.Output.Value, _ = json.Marshal(value)
			node.StartedAt, node.FinishedAt = start.Add(5*time.Second), start.Add(6*time.Second)
		} else {
			newCall.Nodes = append(newCall.Nodes, spec)
			node.Status, node.Error, node.Output = "succeeded", "", workergraph.Output{}
			node.StartedAt, node.FinishedAt = start.Add(6*time.Second), start.Add(7*time.Second)
			for _, name := range []string{"stdout.log", "report.json"} {
				path := newBase + "nodes/join/" + name
				files[path] = []byte(`{"joined":true}`)
				node.Output.Artifacts = append(node.Output.Artifacts, workergraph.Artifact{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(files[path]))})
			}
			node.Output.Value, _ = json.Marshal(map[string]any{"stdout": "joined", "output_path": newBase + "nodes/join/stdout.log", "exit_code": 0})
			files[releaseAuditRoot+"android/report.json"] = files[newBase+"nodes/join/report.json"]
		}
		newCheckpoint.Nodes = append(newCheckpoint.Nodes, node)
	}
	view := func(node workergraph.NodeState) map[string]any {
		raw, _ := json.Marshal(node)
		var output map[string]any
		_ = json.Unmarshal(raw, &output)
		named := map[string]workergraph.Artifact{}
		for _, artifact := range node.Output.Artifacts {
			if filepath.Base(artifact.Path) != "stdout.log" {
				named[filepath.Base(artifact.Path)] = artifact
			}
		}
		output["output"].(map[string]any)["files"] = named
		return output
	}
	for _, graph := range []struct {
		base       string
		call       releaseAuditReuseCall
		checkpoint workergraph.Checkpoint
	}{{oldBase, oldCall, oldCheckpoint}, {newBase, newCall, newCheckpoint}} {
		save(graph.base+"graph.json", graph.checkpoint)
		var nodeViews []map[string]any
		for _, node := range graph.checkpoint.Nodes {
			nodeViews = append(nodeViews, view(node))
		}
		for _, spec := range graph.call.Nodes {
			if spec.Kind == "reuse" {
				continue
			}
			dependencies := []map[string]any{}
			for _, dep := range spec.DependsOn {
				for _, node := range graph.checkpoint.Nodes {
					if node.ID == dep.ID {
						dependencies = append(dependencies, view(node))
					}
				}
			}
			save(graph.base+"nodes/"+spec.ID+"/dependencies.json", dependencies)
		}
		call, _ := json.Marshal(graph.call)
		receipt, _ := json.Marshal(map[string]any{"key": graph.call.Key, "status": graph.checkpoint.Status, "nodes": nodeViews})
		content, _ := json.Marshal(string(receipt))
		for _, message := range []agent.Message{{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: graph.call.Key, Name: "run_graph", Input: call}}}, {Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: graph.call.Key, Content: content}}}} {
			raw, _ := json.Marshal(agent.Event{Type: "message", Message: &message})
			files[runBase+"events.jsonl"] = append(files[runBase+"events.jsonl"], append(raw, '\n')...)
		}
	}
	childBase := oldBase + "nodes/manifest/"
	history := []agent.Message{{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "child-inspection", Content: json.RawMessage(`"inspected"`)}}}}
	childEvent, _ := json.Marshal(agent.Event{Type: "message", At: start.Add(2 * time.Second).Format(time.RFC3339Nano), Message: &history[0]})
	files[childBase+"events.jsonl"] = append(childEvent, '\n')
	save(childBase+"session.json", map[string]any{"run_id": "audit-run", "graph_key": "release-android", "node_id": "manifest", "result": "done", "history": history, "log_checkpoint": map[string]any{"offset": len(files[childBase+"events.jsonl"]), "sha256": fmt.Sprintf("%x", sha256.Sum256(files[childBase+"events.jsonl"]))}})
	return files
}

func TestReleaseAuditReuseGraphRejectsRepeatedOrUnboundWork(t *testing.T) {
	base := "/workspace/.pwnmesh/runs/audit-run/graph-tools/"
	for name, mutate := range map[string]func(map[string][]byte){
		"valid": func(map[string][]byte) {},
		"source bytes changed": func(files map[string][]byte) {
			files[base+"release-android/nodes/manifest/manifests.json"] = []byte(`{"changed":true}`)
		},
		"repeated child Agent": func(files map[string][]byte) {
			files[base+"release-android-recovered/nodes/manifest/session.json"] = []byte(`{}`)
		},
		"source child journal changed": func(files map[string][]byte) {
			files[base+"release-android/nodes/manifest/events.jsonl"] = append(files[base+"release-android/nodes/manifest/events.jsonl"], []byte("{}\n")...)
		},
		"frozen dependency missing": func(files map[string][]byte) {
			delete(files, base+"release-android-recovered/nodes/join/dependencies.json")
		},
		"dependency has only legacy positional artifacts": func(files map[string][]byte) {
			path := base + "release-android-recovered/nodes/join/dependencies.json"
			var dependencies []map[string]any
			_ = json.Unmarshal(files[path], &dependencies)
			for _, dependency := range dependencies {
				delete(dependency["output"].(map[string]any), "files")
			}
			files[path], _ = json.Marshal(dependencies)
		},
		"report differs from joined artifact": func(files map[string][]byte) {
			files[releaseAuditRoot+"android/report.json"] = []byte(`{"different":true}`)
		},
		"original failure erased": func(files map[string][]byte) {
			delete(files, base+"release-android/graph.json")
		},
		"reuse provenance removed": func(files map[string][]byte) {
			path := base + "release-android-recovered/graph.json"
			var checkpoint workergraph.Checkpoint
			_ = json.Unmarshal(files[path], &checkpoint)
			var value map[string]any
			_ = json.Unmarshal(checkpoint.Nodes[0].Output.Value, &value)
			delete(value, "reused_from")
			checkpoint.Nodes[0].Output.Value, _ = json.Marshal(value)
			files[path], _ = json.Marshal(checkpoint)
		},
	} {
		t.Run(name, func(t *testing.T) {
			files := releaseAuditReuseGraphFixture(t)
			mutate(files)
			failures := validateReleaseAuditReuseGraph("audit-run", "android", files)
			if name == "valid" && len(failures) != 0 || name != "valid" && len(failures) == 0 {
				t.Fatalf("unexpected acceptance: %v", failures)
			}
		})
	}
}

func TestReleaseAuditReuseProvenanceBindsSourceIdentityKindAndOutputs(t *testing.T) {
	files := releaseAuditReuseGraphFixture(t)
	base := "/workspace/.pwnmesh/runs/audit-run/graph-tools/"
	var source, recovered workergraph.Checkpoint
	_ = json.Unmarshal(files[base+"release-android/graph.json"], &source)
	_ = json.Unmarshal(files[base+"release-android-recovered/graph.json"], &recovered)
	original, imported := source.Nodes[1], recovered.Nodes[1]
	if original.Kind != "agent" || !releaseAuditReuseOutputMatches(original, imported, "release-android") {
		t.Fatal("valid Agent import rejected")
	}
	for _, field := range []string{"key", "node", "kind", "definition_sha256", "input_sha256"} {
		t.Run(field, func(t *testing.T) {
			changed := imported
			var value map[string]any
			_ = json.Unmarshal(changed.Output.Value, &value)
			value["reused_from"].(map[string]any)[field] = "wrong"
			changed.Output.Value, _ = json.Marshal(value)
			if releaseAuditReuseOutputMatches(original, changed, "release-android") {
				t.Fatal("changed source identity accepted")
			}
		})
	}
	changed := imported
	changed.Kind = "function"
	if releaseAuditReuseOutputMatches(original, changed, "release-android") {
		t.Fatal("Agent output accepted as command evidence")
	}
	changed = imported
	changed.Output.Artifacts = append([]workergraph.Artifact(nil), imported.Output.Artifacts...)
	changed.Output.Artifacts[0].SHA256 = strings.Repeat("0", 64)
	if releaseAuditReuseOutputMatches(original, changed, "release-android") {
		t.Fatal("changed imported artifact accepted")
	}
}
