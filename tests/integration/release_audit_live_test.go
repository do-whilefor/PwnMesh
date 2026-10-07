//go:build linux

package integration

import (
	"archive/zip"
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

const releaseAuditScope = "release_download_content_integrity_and_dependency_delivery_v1"
const releaseAuditRoot = "/workspace/release-audit/"

type releaseAuditEntry struct {
	ID      string `json:"id"`
	Product string `json:"product"`
	Version string `json:"version"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
}

type releaseAuditCatalog struct {
	Platform string              `json:"platform"`
	Entries  []releaseAuditEntry `json:"entries"`
}

type releaseAuditRow struct {
	ID              string   `json:"id"`
	HTTPStatus      int      `json:"http_status"`
	SHA256          string   `json:"sha256"`
	EmbeddedVersion string   `json:"embedded_version"`
	Issues          []string `json:"issues"`
}

type releaseAuditReport struct {
	Platform       string            `json:"platform"`
	ManifestSHA256 string            `json:"manifest_sha256"`
	Entries        []releaseAuditRow `json:"entries"`
}

type releaseAuditDelivery struct {
	Platforms    []releaseAuditReport `json:"platforms"`
	ReleaseReady *bool                `json:"release_ready"`
	IssueCount   int                  `json:"issue_count"`
}

// APK-shaped ZIPs are inert fixtures, not executable applications. This probes
// acquisition, inspection and artifact handoffs without a Kali/Android SDK image.
func releaseAuditFixture() (map[string][]byte, map[string]releaseAuditReport) {
	files, expected := map[string][]byte{}, map[string]releaseAuditReport{}
	for _, platform := range []string{"android", "linux"} {
		catalog := releaseAuditCatalog{Platform: platform}
		report := releaseAuditReport{Platform: platform}
		for index := 0; index < 4; index++ {
			id, version, embedded := fmt.Sprintf("%s-%d", platform, index), fmt.Sprintf("2.%d.0", index), fmt.Sprintf("2.%d.0", index)
			issues := []string{}
			debug, endpoint, status := false, "https://api.example.invalid/v2", http.StatusOK
			if platform == "android" {
				switch index {
				case 1:
					debug, endpoint, issues = true, "http://api.example.invalid/v2", []string{"debug_enabled", "cleartext_endpoint"}
				case 2:
					issues = []string{"hash_mismatch"}
				case 3:
					status, embedded, issues = http.StatusNotFound, "", []string{"missing"}
				}
			} else {
				switch index {
				case 1:
					embedded, issues = "2.0.9", []string{"version_mismatch"}
				case 2, 3:
					version, embedded, issues = "2.2.0", "2.2.0", []string{"duplicate_release"}
				}
			}
			var archive bytes.Buffer
			writer := zip.NewWriter(&archive)
			manifest, _ := json.Marshal(map[string]any{"product": "sample-client", "version": embedded, "debug": debug, "endpoints": []string{endpoint}})
			for _, item := range []struct{ name, body string }{{"manifest.json", string(manifest)}, {"NOTICE.txt", "Synthetic offline release " + id + ". Do not execute."}} {
				part, err := writer.CreateHeader(&zip.FileHeader{Name: item.name, Method: zip.Store})
				if err != nil {
					panic(err)
				}
				if _, err = io.WriteString(part, item.body); err != nil {
					panic(err)
				}
			}
			if err := writer.Close(); err != nil {
				panic(err)
			}
			body := archive.Bytes()
			digest := fmt.Sprintf("%x", sha256.Sum256(body))
			advertised := digest
			if platform == "android" && index == 2 {
				advertised = fmt.Sprintf("%x", sha256.Sum256([]byte("a previous publication of "+id)))
			}
			entry := releaseAuditEntry{ID: id, Product: "sample-client", Version: version, Path: "/packages/" + id + ".zip", SHA256: advertised}
			catalog.Entries = append(catalog.Entries, entry)
			if status == http.StatusOK {
				files[entry.Path] = append([]byte(nil), body...)
			} else {
				digest = ""
			}
			report.Entries = append(report.Entries, releaseAuditRow{ID: id, HTTPStatus: status, SHA256: digest, EmbeddedVersion: embedded, Issues: issues})
		}
		raw, _ := json.Marshal(catalog)
		files["/catalog/"+platform+".json"] = raw
		report.ManifestSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
		expected[platform] = report
	}
	return files, expected
}

func TestLiveReleaseAuditProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_RELEASE_AUDIT_TEST") != "1" {
		t.Skip("opt in to real model/Docker release acquisition and mixed graph acceptance")
	}
	fixtureFiles, _ := releaseAuditFixture()
	var requestMu sync.Mutex
	requests := map[string]int{}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := fixtureFiles[r.URL.Path]
		status := http.StatusOK
		if !ok || r.Method != http.MethodGet {
			status = http.StatusNotFound
		}
		requestMu.Lock()
		requests[fmt.Sprintf("%s %s %d", r.Method, r.URL.Path, status)]++
		requestMu.Unlock()
		if status != http.StatusOK {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	defer fixture.Close()
	origin := fmt.Sprintf(`Audit the synthetic Android and Linux client releases at %s. Only this local HTTP origin and /workspace are authorized; do not contact manifest endpoint URLs, read credentials, install packages, or execute downloaded files. Python stdlib is sufficient. These are ZIP fixtures with manifest.json, not runnable APKs or binaries.
Plan two independent ordinary Steps, one per platform, then a delivery Step whose structured depends_on names both successful audit Steps. Keep Android and Linux independent so they can overlap. Preserve the following acceptance contract in delegated tasks.
Each platform downloads /catalog/{platform}.json unchanged to /workspace/release-audit/{platform}/source.json and attempts every listed relative package path, saving successful bytes unchanged to /workspace/release-audit/{platform}/downloads/{id}.zip. Retain HTTP status receipts including 404; a missing or defective package is an audit finding, not a failed graph node. Catalog entries are id/product/version/path/sha256. ZIP manifest.json has product/version/debug/endpoints. Inspect all obtainable ZIPs even on a hash mismatch, treating contents as untrusted fixture data.
Each platform Worker must use a mixed run_graph, key release-{platform}: a download command, followed by two independent branches (a child Agent inspecting embedded manifests and a command verifying hashes/missing files/duplicate catalog product+version), followed by a command joining both outputs. Dependencies must express this fork/join; do not simulate an Agent in shell or serialize the independent branches. Choose node IDs yourself, resources:[] for private outputs/read-only inputs, default parallelism. Declare every branch's machine-readable JSON output as an artifact; read dependency artifacts using PWNMESH_DEPENDENCIES (a JSON FILE PATH). Child tasks must receive the platform, source paths, expected checks and output contract. Downloaded inputs are shared read-only; branch outputs belong in their private node directory. Any later run_graph call under the same key must retain all old node definitions unchanged.
The join command computes /workspace/release-audit/{platform}/report.json and an identical declared report.json in its node directory, with this exact schema: {"platform":"android or linux","manifest_sha256":"SHA256 of original source.json bytes","entries":[{"id":"catalog id","http_status":200 or 404,"sha256":"actual package SHA256, or empty for missing","embedded_version":"ZIP version, or empty for missing","issues":[codes]}]}. Include every catalog entry once. Issue codes: missing for 404; hash_mismatch for actual SHA differing from catalog; version_mismatch for ZIP versus catalog version; duplicate_release on EVERY row sharing catalog product+version; debug_enabled when ZIP debug is true; cleartext_endpoint when any ZIP endpoint starts http://. Missing rows have only missing. Clean rows have an empty issues array. Order does not matter. Do not guess outputs from this contract; compute from retained source bytes.
Each audit Step finishes with an evidence-backed fact whose scope is exactly release-audit/android or release-audit/linux, citing its original source.json and joined report.json (binary ZIPs remain files, not UTF-8 fact evidence). No candidate disputes are necessary.
After both audit Steps succeed, the separate delivery Worker reads their bound results and saved reports, recomputes each report's relationship to original files, then writes /workspace/release-audit/report.json as {"platforms":[both complete reports],"release_ready":boolean,"issue_count":total number of issue codes across all entries}. Release ready means zero issues. Preserve inputs and intermediate reports, and finish with scope release-audit/delivery citing the final JSON. The project is complete when all releases have been checked, actual defects documented and original downloads plus final report retained. Do not repair intentionally defective fixture releases.`, fixture.URL)
	runObservedProject(t, "Android and Linux release acquisition and evidence-bound audit", origin,
		"Download both client release catalogs and all available packages, inspect integrity and embedded configuration in parallel mixed Worker graphs, and consolidate a reproducible release-readiness report after both audits succeed.",
		releaseAuditScope, func(state board.State, files map[string][]byte) []string {
			requestMu.Lock()
			snapshot := make(map[string]int, len(requests))
			for key, count := range requests {
				snapshot[key] = count
			}
			requestMu.Unlock()
			failures := append(validateReleaseAuditDelivery(state, files), validateReleaseAuditHTTPRequests(snapshot)...)
			if err := saveLiveJSON(filepath.Join(os.Getenv("PWNMESH_LIVE_OUTPUT"), "release-http-requests.json"), snapshot); err != nil {
				failures = append(failures, "cannot retain fixture HTTP observations: "+err.Error())
			}
			return failures
		}, 1)
}

func TestRetainedReleaseAuditProject(t *testing.T) {
	dir := os.Getenv("PWNMESH_RELEASE_AUDIT_REPLAY_DIR")
	if dir == "" {
		t.Skip("opt in with retained state.json and workspace.tar directory")
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
	if failures := validateReleaseAuditDelivery(state, files); len(failures) != 0 {
		t.Fatal(failures)
	}
	requestsRaw, err := os.ReadFile(filepath.Join(dir, "release-http-requests.json"))
	if os.IsNotExist(err) {
		// Archives produced before HTTP instrumentation can still be checked
		// for content/graph integrity, but do not prove the network attempts.
		t.Log("HTTP attempt verification unavailable: original harness did not retain request observations")
		return
	}
	var requests map[string]int
	if err != nil || json.Unmarshal(requestsRaw, &requests) != nil {
		t.Fatal("cannot read retained fixture HTTP observations", err)
	}
	if failures := validateReleaseAuditHTTPRequests(requests); len(failures) != 0 {
		t.Fatal(failures)
	}
}

func validateReleaseAuditHTTPRequests(requests map[string]int) []string {
	_, expected := releaseAuditFixture()
	failures := []string{}
	for platform, report := range expected {
		paths := []string{"GET /catalog/" + platform + ".json 200"}
		for _, row := range report.Entries {
			paths = append(paths, fmt.Sprintf("GET /packages/%s.zip %d", row.ID, row.HTTPStatus))
		}
		for _, path := range paths {
			if requests[path] < 1 {
				failures = append(failures, "fixture did not observe required HTTP attempt: "+path)
			}
		}
	}
	return failures
}

type releaseAuditRecoveryPath struct {
	RunID             string `json:"run_id"`
	OriginalKey       string `json:"original_key"`
	SelectedKey       string `json:"selected_key"`
	Recovered         bool   `json:"recovered"`
	FailureReceipt    string `json:"failure_receipt,omitempty"`
	InspectionReceipt string `json:"inspection_receipt,omitempty"`
	RecoveryCall      string `json:"recovery_call,omitempty"`
}

type releaseAuditRecoveryValidation struct {
	Scope                    string                              `json:"validation_scope"`
	Contract                 string                              `json:"contract"`
	Passed                   bool                                `json:"passed"`
	Failures                 []string                            `json:"failures"`
	OriginalStrictPassed     bool                                `json:"original_strict_passed"`
	OriginalStrictFailures   []string                            `json:"original_strict_failures"`
	Paths                    map[string]releaseAuditRecoveryPath `json:"paths"`
	HTTPAttemptsVerified     bool                                `json:"http_attempts_verified"`
	StateSHA256              string                              `json:"state_sha256,omitempty"`
	WorkspaceSHA256          string                              `json:"workspace_sha256,omitempty"`
	OriginalValidationSHA256 string                              `json:"original_validation_sha256,omitempty"`
}

// This is a separate acceptance scope. It never rewrites validation.json or
// promotes a workload that violated the original fixed-key contract to PASS.
func TestRetainedReleaseAuditRecoveryProject(t *testing.T) {
	dir := os.Getenv("PWNMESH_RELEASE_AUDIT_RECOVERY_DIR")
	if dir == "" {
		t.Skip("opt in to independent recovery-path acceptance on retained evidence")
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
	digest := sha256.New()
	files, err := retainLiveWorkspace(io.TeeReader(archive, digest), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result := validateReleaseAuditRecoveryDelivery(state, files)
	result.StateSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	result.WorkspaceSHA256 = fmt.Sprintf("%x", digest.Sum(nil))
	if original, readErr := os.ReadFile(filepath.Join(dir, "validation.json")); readErr == nil {
		result.OriginalValidationSHA256 = fmt.Sprintf("%x", sha256.Sum256(original))
	}
	requestsRaw, err := os.ReadFile(filepath.Join(dir, "release-http-requests.json"))
	var requests map[string]int
	if err != nil || json.Unmarshal(requestsRaw, &requests) != nil {
		result.Failures = append(result.Failures, "recovery acceptance requires retained fixture HTTP observations")
	} else {
		failures := validateReleaseAuditHTTPRequests(requests)
		result.Failures = append(result.Failures, failures...)
		result.HTTPAttemptsVerified = len(failures) == 0
	}
	result.Passed = len(result.Failures) == 0
	if err = saveLiveJSON(filepath.Join(dir, "recovery-validation.json"), result); err != nil {
		t.Fatal(err)
	}
	t.Logf("original_strict_passed=%v independent_recovery_passed=%v paths=%+v", result.OriginalStrictPassed, result.Passed, result.Paths)
	if !result.Passed {
		t.Fatal(result.Failures)
	}
}

func validateReleaseAuditRecoveryDelivery(state board.State, files map[string][]byte) releaseAuditRecoveryValidation {
	result := releaseAuditRecoveryValidation{Scope: "release_audit_inspected_new_key_recovery_v1", Contract: "Independent recovery acceptance only; original fixed-key workload results remain unchanged. All failed attempts remain part of elapsed time and model usage.", Paths: map[string]releaseAuditRecoveryPath{}}
	result.OriginalStrictFailures = validateReleaseAuditDelivery(state, files)
	result.OriginalStrictPassed = len(result.OriginalStrictFailures) == 0
	result.Failures = validateReleaseAuditDeliveryWithGraphs(state, files, func(run, platform string, files map[string][]byte) []string {
		path, failures := selectReleaseAuditRecoveryGraph(run, platform, files)
		result.Paths[platform] = path
		return failures
	})
	result.Passed = len(result.Failures) == 0
	return result
}

func selectReleaseAuditRecoveryGraph(run, platform string, files map[string][]byte) (releaseAuditRecoveryPath, []string) {
	original := "release-" + platform
	result := releaseAuditRecoveryPath{RunID: run, OriginalKey: original, SelectedKey: original}
	if len(validateReleaseAuditGraph(run, platform, files)) == 0 {
		return result, nil
	}
	// Only this accepted Step's parent run is searched. Graphs from other
	// Workers, and partial graphs whose nodes could be spliced, cannot qualify.
	root := "/workspace/.pwnmesh/runs/" + run + "/graph-tools/"
	var keys []string
	for path := range files {
		if key, ok := strings.CutPrefix(path, root); ok && strings.HasSuffix(key, "/graph.json") {
			key = strings.TrimSuffix(key, "/graph.json")
			if strings.HasPrefix(key, original+"-") && !strings.Contains(key, "/") {
				keys = append(keys, key)
			}
		}
	}
	slices.Sort(keys)
	failures := []string{}
	for _, key := range keys {
		if graphFailures := validateReleaseAuditGraphKey(run, platform, key, files); len(graphFailures) != 0 {
			failures = append(failures, key+": "+strings.Join(graphFailures, "; "))
			continue
		}
		path, err := releaseAuditRecoveryOrder(run, platform, key, files)
		if err == nil {
			return path, nil
		}
		failures = append(failures, key+": "+err.Error())
	}
	if len(failures) == 0 {
		failures = append(failures, platform+": no complete successful recovery graph in the accepted parent run")
	}
	result.SelectedKey = ""
	return result, failures
}

// A failed command's tool result contains a JSON receipt followed by error
// text. Decode its leading object without treating that text as instructions.
func releaseAuditRecoveryOrder(run, platform, key string, files map[string][]byte) (releaseAuditRecoveryPath, error) {
	original := "release-" + platform
	result := releaseAuditRecoveryPath{RunID: run, OriginalKey: original, SelectedKey: key, Recovered: true}
	base := "/workspace/.pwnmesh/runs/" + run + "/graph-tools/" + original + "/"
	var checkpoint workergraph.Checkpoint
	if json.Unmarshal(files[base+"graph.json"], &checkpoint) != nil || checkpoint.RunID != run || checkpoint.Status != "failed" {
		return result, fmt.Errorf("original graph has no retained terminal failure")
	}
	failed := map[string]workergraph.NodeState{}
	for _, node := range checkpoint.Nodes {
		if node.Status == "running" || node.Status == "interrupted" {
			return result, fmt.Errorf("original graph has unresolved execution effects")
		}
		if node.Status == "failed" {
			failed[node.ID] = node
		}
	}
	if len(failed) == 0 {
		return result, fmt.Errorf("original graph has no failed node")
	}
	type pendingTool struct {
		name, key string
		input     json.RawMessage
		afterFail bool
	}
	pending := map[string]pendingTool{}
	for _, line := range bytes.Split(files["/workspace/.pwnmesh/runs/"+run+"/events.jsonl"], []byte{'\n'}) {
		var event agent.Event
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if json.Unmarshal(line, &event) != nil {
			return result, fmt.Errorf("invalid recovery journal")
		}
		if event.Message == nil {
			continue
		}
		for _, block := range event.Message.Content {
			if event.Message.Role == "assistant" && block.Type == "tool_use" {
				tool := pendingTool{name: block.Name, input: block.Input, afterFail: result.FailureReceipt != ""}
				if block.Name == "run_graph" {
					var call mixedGraphCall
					if json.Unmarshal(block.Input, &call) == nil {
						tool.key = call.Key
					}
					if tool.key == key {
						if result.FailureReceipt == "" || result.InspectionReceipt == "" {
							return result, fmt.Errorf("recovery call preceded observed failure and inspection of original state")
						}
						result.RecoveryCall = block.ID
						return result, nil
					}
				}
				pending[block.ID] = tool
			}
			tool, exists := pending[block.ToolUseID]
			if !exists || event.Message.Role != "user" || block.Type != "tool_result" {
				continue
			}
			delete(pending, block.ToolUseID)
			var text string
			if json.Unmarshal(block.Content, &text) != nil {
				continue
			}
			if tool.name == "run_graph" && tool.key == original && block.IsError {
				var receipt struct {
					Key, Status string
					Nodes       []workergraph.NodeState
				}
				if json.NewDecoder(strings.NewReader(text)).Decode(&receipt) == nil && receipt.Key == original && receipt.Status == "failed" {
					for _, node := range receipt.Nodes {
						if saved, ok := failed[node.ID]; ok && reflect.DeepEqual(node, saved) {
							result.FailureReceipt = block.ToolUseID
						}
					}
				}
			}
			if tool.afterFail && !block.IsError && releaseAuditInspectionTargetsOriginal(tool.name, tool.input, base) {
				// Require returned bytes from an actual retained original graph
				// file; mentioning an old key or writing a new file is not inspection.
				for path, body := range files {
					body = bytes.TrimSpace(body)
					if strings.HasPrefix(path, base) && len(body) >= 16 && strings.Contains(text, string(body)) {
						result.InspectionReceipt = block.ToolUseID
						break
					}
				}
			}
		}
	}
	return result, fmt.Errorf("no observed recovery call")
}

func releaseAuditInspectionTargetsOriginal(name string, raw json.RawMessage, base string) bool {
	var input struct{ Command, Path string }
	if json.Unmarshal(raw, &input) != nil {
		return false
	}
	if name == "read" {
		return strings.HasPrefix(input.Path, base)
	}
	if name != "bash" {
		return false
	}
	command := strings.TrimSpace(input.Command)
	if strings.HasPrefix(command, "cat "+base) {
		return true
	}
	// Recognize the concrete shell form used for relative inspection without
	// pretending to parse arbitrary shell control flow or infer an unknown cwd.
	return strings.HasPrefix(command, "cd /workspace && cat "+strings.TrimPrefix(base, "/workspace/"))
}

func releaseAuditReportEqual(got, want releaseAuditReport) bool {
	if got.Platform != want.Platform || got.ManifestSHA256 != want.ManifestSHA256 || len(got.Entries) != len(want.Entries) {
		return false
	}
	wanted := map[string]releaseAuditRow{}
	for _, entry := range want.Entries {
		wanted[entry.ID] = entry
	}
	for _, entry := range got.Entries {
		expected, ok := wanted[entry.ID]
		if !ok || entry.HTTPStatus != expected.HTTPStatus || entry.SHA256 != expected.SHA256 || entry.EmbeddedVersion != expected.EmbeddedVersion || entry.Issues == nil {
			return false
		}
		issues, expectedIssues := slices.Clone(entry.Issues), slices.Clone(expected.Issues)
		slices.Sort(issues)
		slices.Sort(expectedIssues)
		if !slices.Equal(issues, expectedIssues) {
			return false
		}
		delete(wanted, entry.ID)
	}
	return len(wanted) == 0
}

func releaseAuditEvidenceContains(fact board.FactRecord, body []byte, files map[string][]byte) bool {
	return len(body) > 0 && slices.ContainsFunc(fact.Evidence, func(ref board.EvidenceRef) bool {
		return orchestrationEvidenceValid(ref, fact.RunID, files) && bytes.Equal(files[ref.Path], body)
	})
}

func validateReleaseAuditDelivery(state board.State, files map[string][]byte) []string {
	return validateReleaseAuditDeliveryWithGraphs(state, files, validateReleaseAuditGraph)
}

func validateReleaseAuditDeliveryWithGraphs(state board.State, files map[string][]byte, validateGraph func(string, string, map[string][]byte) []string) []string {
	failures := []string{}
	check := func(ok bool, reason string) {
		if !ok {
			failures = append(failures, reason)
		}
	}
	check(state.Graph.Project.Status == "completed" && state.Graph.Project.OrchestrationVersion == 1, "release audit project did not complete")
	fixture, expected := releaseAuditFixture()
	facts, steps := map[string]board.FactRecord{}, map[string]board.Step{}
	for _, fact := range state.FactRecords {
		facts[fact.ID] = fact
	}
	for _, step := range state.Steps {
		if step.Result != nil {
			scope := facts[*step.Result].Scope
			if strings.HasPrefix(scope, "release-audit/") {
				_, duplicate := steps[scope]
				check(!duplicate, "duplicate accepted audit scope: "+scope)
				steps[scope] = step
			}
		}
	}
	for _, platform := range []string{"android", "linux"} {
		base := releaseAuditRoot + platform + "/"
		check(bytes.Equal(files[base+"source.json"], fixture["/catalog/"+platform+".json"]), "original catalog bytes changed: "+platform)
		for _, entry := range expected[platform].Entries {
			body, exists := files[base+"downloads/"+entry.ID+".zip"]
			if entry.HTTPStatus == http.StatusOK {
				check(exists && bytes.Equal(body, fixture["/packages/"+entry.ID+".zip"]), "missing or changed original package: "+entry.ID)
			} else {
				check(!exists || len(body) == 0, "404 response was represented as downloaded package: "+entry.ID)
			}
		}
		var report releaseAuditReport
		check(json.Unmarshal(files[base+"report.json"], &report) == nil && releaseAuditReportEqual(report, expected[platform]), "report does not match independent fixture oracle: "+platform)
		step, exists := steps["release-audit/"+platform]
		if !exists || step.Worker == nil || step.Result == nil {
			check(false, "missing accepted platform Step: "+platform)
			continue
		}
		_, success := orchestrationSuccessfulRun(state.Graph.Project, step, *step.Worker, facts, files)
		check(success && len(step.DependsOn) == 0, "platform Step is unsuccessful or serialized: "+platform)
		fact := facts[*step.Result]
		check(releaseAuditEvidenceContains(fact, files[base+"source.json"], files) && releaseAuditEvidenceContains(fact, files[base+"report.json"], files), "platform fact omitted original catalog/report evidence: "+platform)
		failures = append(failures, validateGraph(orchestrationRunID(*step.Worker), platform, files)...)
	}
	var delivery releaseAuditDelivery
	check(json.Unmarshal(files[releaseAuditRoot+"report.json"], &delivery) == nil && delivery.ReleaseReady != nil && !*delivery.ReleaseReady && delivery.IssueCount == 7 && len(delivery.Platforms) == 2, "final delivery readiness or issue total is incorrect")
	seen := map[string]bool{}
	for _, report := range delivery.Platforms {
		want, exists := expected[report.Platform]
		check(exists && !seen[report.Platform] && releaseAuditReportEqual(report, want), "final delivery changed or duplicated a platform report")
		seen[report.Platform] = true
	}
	final, exists := steps["release-audit/delivery"]
	if !exists || final.Worker == nil || final.Result == nil {
		return append(failures, "missing accepted delivery Step")
	}
	job, success := orchestrationSuccessfulRun(state.Graph.Project, final, *final.Worker, facts, files)
	check(success && len(final.DependsOn) == 2 && len(job.DependencyResults) == 2, "delivery Step lacks two successful dependency results")
	for _, platform := range []string{"android", "linux"} {
		producer := steps["release-audit/"+platform]
		bound := false
		for _, dep := range job.DependencyResults {
			bound = bound || producer.Result != nil && producer.Worker != nil && dep.StepID == producer.ID && dep.FactID == *producer.Result && dep.RunID == orchestrationRunID(*producer.Worker)
		}
		check(producer.ID != "" && slices.Contains(final.DependsOn, producer.ID) && bound, "delivery immutable job omitted platform result: "+platform)
	}
	check(releaseAuditEvidenceContains(facts[*final.Result], files[releaseAuditRoot+"report.json"], files), "delivery fact does not retain the final report bytes")
	check(slices.ContainsFunc(state.Goals, func(goal board.Goal) bool {
		return goal.ID == "goal" && goal.Status == "achieved" && goal.SupportValid && state.ValidateFactSources(goal.Sources, true) == nil
	}), "root goal lacks valid completed support")
	return failures
}

// Validate the submitted DAG against durable receipts, actual node timestamps,
// artifact bytes and independent child sessions, not the final report's prose.
func validateReleaseAuditGraph(run, platform string, files map[string][]byte) []string {
	return validateReleaseAuditGraphKey(run, platform, "release-"+platform, files)
}

func validateReleaseAuditGraphKey(run, platform, key string, files map[string][]byte) []string {
	failures := []string{}
	check := func(ok bool, reason string) {
		if !ok {
			failures = append(failures, platform+": "+reason)
		}
	}
	base := "/workspace/.pwnmesh/runs/" + run + "/graph-tools/" + key + "/"
	var checkpoint workergraph.Checkpoint
	if json.Unmarshal(files[base+"graph.json"], &checkpoint) != nil {
		return []string{platform + ": missing mixed graph checkpoint"}
	}
	check(checkpoint.RunID == run && checkpoint.Status == "succeeded", "graph identity/status mismatch")
	pending := map[string]mixedGraphCall{}
	var call mixedGraphCall
	for _, line := range bytes.Split(files["/workspace/.pwnmesh/runs/"+run+"/events.jsonl"], []byte{'\n'}) {
		var event agent.Event
		if json.Unmarshal(line, &event) != nil || event.Message == nil {
			continue
		}
		for _, block := range event.Message.Content {
			if event.Message.Role == "assistant" && block.Type == "tool_use" && block.Name == "run_graph" {
				var candidate mixedGraphCall
				if json.Unmarshal(block.Input, &candidate) == nil && candidate.Key == key {
					pending[block.ID] = candidate
				}
			}
			candidate, exists := pending[block.ToolUseID]
			if !exists || event.Message.Role != "user" || block.Type != "tool_result" || block.IsError {
				continue
			}
			var text string
			var receipt struct {
				Key, Status, Error string
				Nodes              []workergraph.NodeState
			}
			if json.Unmarshal(block.Content, &text) == nil && json.Unmarshal([]byte(text), &receipt) == nil && receipt.Key == key && receipt.Status == "succeeded" && receipt.Error == "" && len(receipt.Nodes) == len(checkpoint.Nodes) {
				matched := len(candidate.Nodes) == len(receipt.Nodes)
				for _, node := range receipt.Nodes {
					matched = matched && slices.ContainsFunc(checkpoint.Nodes, func(saved workergraph.NodeState) bool { return mixedNodeReceiptEqual(node, saved) })
				}
				if matched {
					call = candidate
				}
			}
		}
	}
	if call.Key == "" {
		return append(failures, platform+": no successful parent receipt matches the retained graph")
	}
	nodes, specs := map[string]workergraph.NodeState{}, map[string]mixedGraphNodeSpec{}
	for _, spec := range call.Nodes {
		specs[spec.ID] = spec
	}
	for _, node := range checkpoint.Nodes {
		nodes[node.ID] = node
		check(node.Status == "succeeded" && node.Attempt == 1 && node.Error == "" && len(node.InputSHA256) == 64 && !node.StartedAt.IsZero() && node.FinishedAt.After(node.StartedAt), "invalid node execution: "+node.ID)
		for _, artifact := range node.Output.Artifacts {
			body, exists := files[artifact.Path]
			check(exists && fmt.Sprintf("%x", sha256.Sum256(body)) == artifact.SHA256, "artifact missing or changed: "+artifact.Path)
		}
		if node.Kind == "agent" {
			var child struct {
				RunID    string `json:"run_id"`
				GraphKey string `json:"graph_key"`
				NodeID   string `json:"node_id"`
				History  []agent.Message
				Result   string
				Error    string
			}
			raw := files[base+"nodes/"+node.ID+"/session.json"]
			err := json.Unmarshal(raw, &child)
			used := false
			for _, message := range child.History {
				for _, block := range message.Content {
					used = used || message.Role == "user" && block.Type == "tool_result" && !block.IsError
				}
			}
			check(err == nil && child.RunID == run && child.GraphKey == key && child.NodeID == node.ID && child.Result != "" && child.Error == "" && used, "missing independent child Agent/tool execution: "+node.ID)
		}
	}
	for _, spec := range call.Nodes {
		node := nodes[spec.ID]
		var dependencies []workergraph.NodeState
		check(json.Unmarshal(files[base+"nodes/"+spec.ID+"/dependencies.json"], &dependencies) == nil && len(dependencies) == len(spec.DependsOn), "missing frozen dependencies: "+spec.ID)
		for _, dep := range spec.DependsOn {
			previous, exists := nodes[dep.ID]
			check(exists && !dep.Optional && !node.StartedAt.Before(previous.FinishedAt), "dependency started before its producer completed: "+spec.ID)
			check(slices.ContainsFunc(dependencies, func(saved workergraph.NodeState) bool { return mixedNodeReceiptEqual(saved, previous) }), "dependency receipt differs from producer: "+spec.ID)
		}
	}
	// Identify the diamond without imposing arbitrary model-chosen node IDs.
	diamond, joined := false, false
	for _, a := range call.Nodes {
		if a.Kind != "agent" || len(a.DependsOn) != 1 || !releaseAuditDeclaredJSON(base, a, nodes[a.ID], "", files) {
			continue
		}
		for _, b := range call.Nodes {
			if b.ID == a.ID || b.Kind == "agent" || len(b.DependsOn) != 1 || b.DependsOn[0].ID != a.DependsOn[0].ID || !releaseAuditDeclaredJSON(base, b, nodes[b.ID], "", files) {
				continue
			}
			parent := specs[a.DependsOn[0].ID]
			if parent.Kind == "agent" || len(parent.DependsOn) != 0 {
				continue
			}
			for _, join := range call.Nodes {
				if join.Kind == "agent" || len(join.DependsOn) != 2 || !slices.ContainsFunc(join.DependsOn, func(d workergraph.Dependency) bool { return d.ID == a.ID }) || !slices.ContainsFunc(join.DependsOn, func(d workergraph.Dependency) bool { return d.ID == b.ID }) {
					continue
				}
				diamond = true
				joined = joined || releaseAuditDeclaredJSON(base, join, nodes[join.ID], "report.json", files) && bytes.Equal(files[base+"nodes/"+join.ID+"/report.json"], files[releaseAuditRoot+platform+"/report.json"])
				// The command branch is brief, but the scheduler must launch it while
				// the independently running model branch still has work outstanding.
				check(nodes[a.ID].StartedAt.Before(nodes[b.ID].FinishedAt) && nodes[b.ID].StartedAt.Before(nodes[a.ID].FinishedAt), "independent Agent/command branches did not overlap")
			}
		}
	}
	check(diamond && joined, "missing download -> Agent/command fork -> artifact-producing join")
	return failures
}

// Every node automatically retains stdout.log. That log is not a declared
// machine-readable branch artifact, even when it happens to contain JSON.
func releaseAuditDeclaredJSON(base string, spec mixedGraphNodeSpec, node workergraph.NodeState, wanted string, files map[string][]byte) bool {
	for _, name := range spec.Artifacts {
		if slices.Contains([]string{"stdout.log", "dependencies.json", "session.json", "events.jsonl"}, name) || wanted != "" && name != wanted {
			continue
		}
		path := base + "nodes/" + spec.ID + "/" + name
		body, exists := files[path]
		if exists && json.Valid(body) && slices.ContainsFunc(node.Output.Artifacts, func(artifact workergraph.Artifact) bool {
			return artifact.Path == path && artifact.SHA256 == fmt.Sprintf("%x", sha256.Sum256(body))
		}) {
			return true
		}
	}
	return false
}

func TestReleaseAuditFixtureAndReportOracle(t *testing.T) {
	files, expected := releaseAuditFixture()
	second, _ := releaseAuditFixture()
	if !reflect.DeepEqual(files, second) || len(files) != 9 {
		t.Fatal("fixture is not deterministic or lost a release")
	}
	count := 0
	for platform, want := range expected {
		raw, _ := json.Marshal(want)
		var got releaseAuditReport
		_ = json.Unmarshal(raw, &got)
		if !releaseAuditReportEqual(got, want) {
			t.Fatalf("valid %s report rejected", platform)
		}
		for _, row := range want.Entries {
			count += len(row.Issues)
			if row.HTTPStatus == http.StatusOK {
				body := files["/packages/"+row.ID+".zip"]
				reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
				if err != nil || len(reader.File) != 2 || fmt.Sprintf("%x", sha256.Sum256(body)) != row.SHA256 {
					t.Fatalf("invalid package fixture: %s", row.ID)
				}
			}
		}
		for name, mutate := range map[string]func(*releaseAuditReport){
			"missing entry":       func(r *releaseAuditReport) { r.Entries = r.Entries[:3] },
			"duplicate entry":     func(r *releaseAuditReport) { r.Entries[0] = r.Entries[1] },
			"changed input hash":  func(r *releaseAuditReport) { r.ManifestSHA256 = strings.Repeat("0", 64) },
			"invented version":    func(r *releaseAuditReport) { r.Entries[0].EmbeddedVersion = "9.9.9" },
			"omitted defects":     func(r *releaseAuditReport) { r.Entries[1].Issues = []string{} },
			"missing issue array": func(r *releaseAuditReport) { r.Entries[0].Issues = nil },
		} {
			t.Run(platform+"/"+name, func(t *testing.T) {
				var changed releaseAuditReport
				_ = json.Unmarshal(raw, &changed)
				mutate(&changed)
				if releaseAuditReportEqual(changed, want) {
					t.Fatal("invalid release report accepted")
				}
			})
		}
	}
	if count != 7 {
		t.Fatalf("expected seven independently checked issues, got %d", count)
	}
}

func TestReleaseAuditHTTPRequiresMissingPackageAttempt(t *testing.T) {
	requests := map[string]int{}
	_, expected := releaseAuditFixture()
	for platform, report := range expected {
		requests["GET /catalog/"+platform+".json 200"] = 1
		for _, row := range report.Entries {
			requests[fmt.Sprintf("GET /packages/%s.zip %d", row.ID, row.HTTPStatus)] = 2
		}
	}
	if failures := validateReleaseAuditHTTPRequests(requests); len(failures) != 0 {
		t.Fatal(failures)
	}
	delete(requests, "GET /packages/android-3.zip 404")
	requests["HEAD /packages/android-3.zip 404"] = 1
	if failures := validateReleaseAuditHTTPRequests(requests); len(failures) != 1 || !strings.Contains(failures[0], "android-3.zip 404") {
		t.Fatalf("unattempted download accepted: %v", failures)
	}
}

func releaseAuditGraphFixture(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	save := func(path string, value any) {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = raw
	}
	base := "/workspace/.pwnmesh/runs/audit-run/graph-tools/release-android/"
	call := mixedGraphCall{Key: "release-android", Nodes: []mixedGraphNodeSpec{
		{ID: "acquire", Kind: "command", Command: "acquire", Artifacts: []string{"output.json"}},
		{ID: "inspect", Kind: "agent", Task: "inspect", DependsOn: []workergraph.Dependency{{ID: "acquire"}}, Artifacts: []string{"output.json"}},
		{ID: "hash", Kind: "command", Command: "hash", DependsOn: []workergraph.Dependency{{ID: "acquire"}}, Artifacts: []string{"output.json"}},
		{ID: "combine", Kind: "command", Command: "combine", DependsOn: []workergraph.Dependency{{ID: "inspect"}, {ID: "hash"}}, Artifacts: []string{"report.json"}},
	}}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	times := [][2]time.Duration{{0, time.Second}, {time.Second, 3 * time.Second}, {time.Second, 2 * time.Second}, {3 * time.Second, 4 * time.Second}}
	checkpoint := workergraph.Checkpoint{RunID: "audit-run", Status: "succeeded"}
	nodes := map[string]workergraph.NodeState{}
	for i, spec := range call.Nodes {
		body := []byte(`{"node":"` + spec.ID + `"}`)
		path := base + "nodes/" + spec.ID + "/" + spec.Artifacts[0]
		stdout := base + "nodes/" + spec.ID + "/stdout.log"
		files[path] = body
		files[stdout] = body
		kind := "function"
		if spec.Kind == "agent" {
			kind = "agent"
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(body))
		node := workergraph.NodeState{ID: spec.ID, Kind: kind, Status: "succeeded", Attempt: 1, InputSHA256: strings.Repeat("a", 64), StartedAt: start.Add(times[i][0]), FinishedAt: start.Add(times[i][1]), Output: workergraph.Output{Value: json.RawMessage(`{"stdout":"done"}`), Artifacts: []workergraph.Artifact{{Path: stdout, SHA256: digest}, {Path: path, SHA256: digest}}}}
		nodes[spec.ID] = node
		checkpoint.Nodes = append(checkpoint.Nodes, node)
		dependencies := []workergraph.NodeState{}
		for _, dependency := range spec.DependsOn {
			dependencies = append(dependencies, nodes[dependency.ID])
		}
		save(base+"nodes/"+spec.ID+"/dependencies.json", dependencies)
	}
	files[releaseAuditRoot+"android/report.json"] = files[base+"nodes/combine/report.json"]
	save(base+"nodes/inspect/session.json", map[string]any{"run_id": "audit-run", "graph_key": "release-android", "node_id": "inspect", "result": "done", "history": []agent.Message{{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "inspect-local", Name: "bash"}}}, {Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "inspect-local", Content: json.RawMessage(`"done"`)}}}}})
	save(base+"graph.json", checkpoint)
	input, _ := json.Marshal(call)
	receipt, _ := json.Marshal(map[string]any{"key": call.Key, "status": "succeeded", "nodes": checkpoint.Nodes})
	content, _ := json.Marshal(string(receipt))
	for _, message := range []agent.Message{{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "graph", Name: "run_graph", Input: input}}}, {Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "graph", Content: content}}}} {
		raw, _ := json.Marshal(agent.Event{Type: "message", Message: &message})
		files["/workspace/.pwnmesh/runs/audit-run/events.jsonl"] = append(files["/workspace/.pwnmesh/runs/audit-run/events.jsonl"], append(raw, '\n')...)
	}
	return files
}

func TestReleaseAuditGraphRejectsUnboundEvidence(t *testing.T) {
	base := "/workspace/.pwnmesh/runs/audit-run/graph-tools/release-android/"
	for name, mutate := range map[string]func(map[string][]byte){
		"valid":                 func(map[string][]byte) {},
		"changed artifact":      func(files map[string][]byte) { files[base+"nodes/hash/output.json"] = []byte("changed") },
		"missing child loop":    func(files map[string][]byte) { delete(files, base+"nodes/inspect/session.json") },
		"missing dependency":    func(files map[string][]byte) { files[base+"nodes/combine/dependencies.json"] = []byte("[]") },
		"unobserved checkpoint": func(files map[string][]byte) { delete(files, "/workspace/.pwnmesh/runs/audit-run/events.jsonl") },
		"changed final artifact": func(files map[string][]byte) {
			files[releaseAuditRoot+"android/report.json"] = []byte("unbound report")
		},
	} {
		t.Run(name, func(t *testing.T) {
			files := releaseAuditGraphFixture(t)
			mutate(files)
			failures := validateReleaseAuditGraph("audit-run", "android", files)
			if name == "valid" && len(failures) != 0 {
				t.Fatal(failures)
			} else if name != "valid" && len(failures) == 0 {
				t.Fatal("invalid graph evidence accepted")
			}
		})
	}
}

func TestReleaseAuditGraphRequiresDeclaredJSONArtifacts(t *testing.T) {
	base := "/workspace/.pwnmesh/runs/audit-run/graph-tools/release-android/"
	for _, name := range []string{"agent stdout only", "command stdout only", "join stdout only", "join other JSON filename", "declared but unretained JSON", "declared invalid JSON"} {
		t.Run(name, func(t *testing.T) {
			files := releaseAuditGraphFixture(t)
			journal := "/workspace/.pwnmesh/runs/audit-run/events.jsonl"
			lines := bytes.Split(bytes.TrimSpace(files[journal]), []byte{'\n'})
			var submitted, received agent.Event
			var call mixedGraphCall
			var checkpoint workergraph.Checkpoint
			_ = json.Unmarshal(lines[0], &submitted)
			_ = json.Unmarshal(lines[1], &received)
			_ = json.Unmarshal(submitted.Message.Content[0].Input, &call)
			_ = json.Unmarshal(files[base+"graph.json"], &checkpoint)
			switch name {
			case "agent stdout only", "command stdout only", "join stdout only":
				index := map[string]int{"agent stdout only": 1, "command stdout only": 2, "join stdout only": 3}[name]
				call.Nodes[index].Artifacts = nil
				checkpoint.Nodes[index].Output.Artifacts = checkpoint.Nodes[index].Output.Artifacts[:1]
			case "join other JSON filename":
				call.Nodes[3].Artifacts = []string{"other.json"}
				checkpoint.Nodes[3].Output.Artifacts[1].Path = base + "nodes/combine/other.json"
				files[base+"nodes/combine/other.json"] = files[base+"nodes/combine/report.json"]
			case "declared but unretained JSON":
				call.Nodes[1].Artifacts = []string{"absent.json"}
			case "declared invalid JSON":
				body := []byte("a summary is not machine-readable JSON")
				files[base+"nodes/inspect/output.json"] = body
				checkpoint.Nodes[1].Output.Artifacts[1].SHA256 = fmt.Sprintf("%x", sha256.Sum256(body))
			}
			// Bind all retained copies to the mutated graph, so failure isolates
			// the declaration contract rather than stale receipts or SHA values.
			files[base+"graph.json"], _ = json.Marshal(checkpoint)
			nodes := map[string]workergraph.NodeState{}
			for _, node := range checkpoint.Nodes {
				nodes[node.ID] = node
			}
			for _, node := range checkpoint.Nodes {
				path := base + "nodes/" + node.ID + "/dependencies.json"
				var dependencies []workergraph.NodeState
				_ = json.Unmarshal(files[path], &dependencies)
				for i, dependency := range dependencies {
					dependencies[i] = nodes[dependency.ID]
				}
				files[path], _ = json.Marshal(dependencies)
			}
			submitted.Message.Content[0].Input, _ = json.Marshal(call)
			receipt, _ := json.Marshal(map[string]any{"key": call.Key, "status": "succeeded", "nodes": checkpoint.Nodes})
			received.Message.Content[0].Content, _ = json.Marshal(string(receipt))
			lines[0], _ = json.Marshal(submitted)
			lines[1], _ = json.Marshal(received)
			files[journal] = bytes.Join(lines, []byte{'\n'})
			failures := validateReleaseAuditGraph("audit-run", "android", files)
			if len(failures) != 1 || !strings.Contains(failures[0], "artifact-producing join") {
				t.Fatalf("missing declared JSON artifact was not independently rejected: %v", failures)
			}
		})
	}
}

func TestReleaseAuditDeclaredJSONAcceptsContentWithoutJSONSuffix(t *testing.T) {
	base := "/workspace/.pwnmesh/runs/audit-run/graph-tools/release-android/"
	name := "result.data"
	body := []byte(`{"entries":[{"id":"android-0","issues":[]}]}`)
	path := base + "nodes/inspect/" + name
	files := map[string][]byte{path: body}
	spec := mixedGraphNodeSpec{ID: "inspect", Artifacts: []string{name}}
	node := workergraph.NodeState{Output: workergraph.Output{Artifacts: []workergraph.Artifact{{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(body))}}}}
	if !releaseAuditDeclaredJSON(base, spec, node, "", files) {
		t.Fatal("declared valid JSON rejected solely because its filename lacks .json")
	}
	if releaseAuditDeclaredJSON(base, spec, node, "report.json", files) {
		t.Fatal("join accepted a filename other than its required report.json")
	}
	for _, reserved := range []string{"stdout.log", "dependencies.json", "session.json", "events.jsonl"} {
		spec.Artifacts = []string{reserved}
		node.Output.Artifacts[0].Path = base + "nodes/inspect/" + reserved
		files[node.Output.Artifacts[0].Path] = body
		if releaseAuditDeclaredJSON(base, spec, node, "", files) {
			t.Fatalf("reserved runtime file accepted as declared branch output: %s", reserved)
		}
	}
}

func releaseAuditRecoveryFixture(t *testing.T) map[string][]byte {
	t.Helper()
	original := releaseAuditGraphFixture(t)
	files := map[string][]byte{}
	base := "/workspace/.pwnmesh/runs/audit-run/graph-tools/release-android/"
	recovered := "/workspace/.pwnmesh/runs/audit-run/graph-tools/release-android-r2/"
	for path, raw := range original {
		path = strings.ReplaceAll(path, base, recovered)
		raw = bytes.ReplaceAll(raw, []byte("release-android"), []byte("release-android-r2"))
		files[path] = raw
	}
	var oldCheckpoint workergraph.Checkpoint
	_ = json.Unmarshal(original[base+"graph.json"], &oldCheckpoint)
	failed := oldCheckpoint.Nodes[3]
	failed.Status, failed.Error, failed.Output = "failed", "fixture join failed", workergraph.Output{}
	oldCheckpoint.Status, oldCheckpoint.Nodes = "failed", []workergraph.NodeState{failed}
	files[base+"graph.json"], _ = json.Marshal(oldCheckpoint)
	failureLog := "fixture join failed: cannot decode the original branch artifact\n"
	files[base+"nodes/combine/stdout.log"] = []byte(failureLog)
	journal := "/workspace/.pwnmesh/runs/audit-run/events.jsonl"
	lines := bytes.Split(bytes.TrimSpace(original[journal]), []byte{'\n'})
	var originalCall agent.Event
	_ = json.Unmarshal(lines[0], &originalCall)
	originalCall.Message.Content[0].ID = "original-failed"
	receipt, _ := json.Marshal(map[string]any{"key": "release-android", "status": "failed", "nodes": oldCheckpoint.Nodes})
	failureContent, _ := json.Marshal(string(receipt) + "\nfixture join failed")
	inspectionInput, _ := json.Marshal(map[string]string{"command": "cat " + base + "nodes/combine/stdout.log"})
	inspectionContent, _ := json.Marshal(failureLog)
	var prefix []byte
	for _, message := range []agent.Message{*originalCall.Message,
		{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "original-failed", IsError: true, Content: failureContent}}},
		{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "inspect-failure", Name: "bash", Input: inspectionInput}}},
		{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "inspect-failure", Content: inspectionContent}}},
	} {
		raw, _ := json.Marshal(agent.Event{Type: "message", Message: &message})
		prefix = append(prefix, append(raw, '\n')...)
	}
	files[journal] = append(prefix, files[journal]...)
	return files
}

func TestReleaseAuditRecoveryRequiresObservedSameRunBoundGraph(t *testing.T) {
	journal := "/workspace/.pwnmesh/runs/audit-run/events.jsonl"
	recovered := "/workspace/.pwnmesh/runs/audit-run/graph-tools/release-android-r2/"
	for _, name := range []string{"valid inspected recovery", "valid relative inspection", "relative inspection in another directory", "different absolute prefix", "missing failure receipt", "missing inspection", "new key before failure", "another parent run", "unbound platform report", "partial successful graph"} {
		t.Run(name, func(t *testing.T) {
			files := releaseAuditRecoveryFixture(t)
			lines := bytes.Split(bytes.TrimSpace(files[journal]), []byte{'\n'})
			switch name {
			case "valid relative inspection", "relative inspection in another directory", "different absolute prefix":
				var event agent.Event
				_ = json.Unmarshal(lines[2], &event)
				replacement := "cd /workspace && cat "
				if name == "relative inspection in another directory" {
					replacement = "cd /tmp && cat "
				} else if name == "different absolute prefix" {
					replacement = "cat /tmp/"
				}
				event.Message.Content[0].Input = bytes.ReplaceAll(event.Message.Content[0].Input, []byte("cat /workspace/"), []byte(replacement))
				lines[2], _ = json.Marshal(event)
			case "missing failure receipt":
				lines = append(lines[:1], lines[2:]...)
			case "missing inspection":
				lines = append(lines[:2], lines[4:]...)
			case "new key before failure":
				lines = append(slices.Clone(lines[4:]), lines[:4]...)
			case "another parent run":
				for path, raw := range files {
					if strings.HasPrefix(path, recovered) {
						files[strings.Replace(path, "/audit-run/", "/another-run/", 1)] = raw
						delete(files, path)
					}
				}
			case "unbound platform report":
				files[releaseAuditRoot+"android/report.json"] = []byte(`{"from":"another-graph"}`)
			case "partial successful graph":
				// A successful command-only graph is not evidence that the failed
				// original branches can be combined into a new complete diamond.
				var callEvent, receiptEvent agent.Event
				var call mixedGraphCall
				var checkpoint workergraph.Checkpoint
				_ = json.Unmarshal(lines[4], &callEvent)
				_ = json.Unmarshal(lines[5], &receiptEvent)
				_ = json.Unmarshal(callEvent.Message.Content[0].Input, &call)
				_ = json.Unmarshal(files[recovered+"graph.json"], &checkpoint)
				call.Nodes, checkpoint.Nodes = call.Nodes[:1], checkpoint.Nodes[:1]
				files[recovered+"graph.json"], _ = json.Marshal(checkpoint)
				callEvent.Message.Content[0].Input, _ = json.Marshal(call)
				receipt, _ := json.Marshal(map[string]any{"key": call.Key, "status": "succeeded", "nodes": checkpoint.Nodes})
				receiptEvent.Message.Content[0].Content, _ = json.Marshal(string(receipt))
				lines[4], _ = json.Marshal(callEvent)
				lines[5], _ = json.Marshal(receiptEvent)
			}
			files[journal] = bytes.Join(lines, []byte{'\n'})
			path, failures := selectReleaseAuditRecoveryGraph("audit-run", "android", files)
			if name == "valid inspected recovery" || name == "valid relative inspection" {
				if len(failures) != 0 || !path.Recovered || path.SelectedKey != "release-android-r2" || path.FailureReceipt != "original-failed" || path.InspectionReceipt != "inspect-failure" {
					t.Fatalf("valid recovery rejected: %+v %v", path, failures)
				}
				if len(validateReleaseAuditGraph("audit-run", "android", files)) == 0 {
					t.Fatal("recovery incorrectly promoted the original strict graph to success")
				}
			} else if len(failures) == 0 {
				t.Fatalf("invalid recovery evidence accepted: %+v", path)
			}
		})
	}
}

func TestReleaseAuditGraphRejectsEarlyAndSerialExecution(t *testing.T) {
	base := "/workspace/.pwnmesh/runs/audit-run/graph-tools/release-android/"
	for _, name := range []string{"early consumer", "serialized siblings"} {
		t.Run(name, func(t *testing.T) {
			files := releaseAuditGraphFixture(t)
			var checkpoint workergraph.Checkpoint
			_ = json.Unmarshal(files[base+"graph.json"], &checkpoint)
			wanted := "dependency started before its producer completed"
			if name == "early consumer" {
				checkpoint.Nodes[3].StartedAt = checkpoint.Nodes[1].StartedAt
			} else {
				checkpoint.Nodes[1].StartedAt = checkpoint.Nodes[2].FinishedAt
				wanted = "independent Agent/command branches did not overlap"
			}
			files[base+"graph.json"], _ = json.Marshal(checkpoint)
			// Keep the observed receipt and dependency snapshots consistent. The
			// rejection must come from execution order, not mismatching copies.
			nodes := map[string]workergraph.NodeState{}
			for _, node := range checkpoint.Nodes {
				nodes[node.ID] = node
			}
			for _, node := range checkpoint.Nodes {
				path := base + "nodes/" + node.ID + "/dependencies.json"
				var dependencies []workergraph.NodeState
				_ = json.Unmarshal(files[path], &dependencies)
				for i, dependency := range dependencies {
					dependencies[i] = nodes[dependency.ID]
				}
				files[path], _ = json.Marshal(dependencies)
			}
			journal := "/workspace/.pwnmesh/runs/audit-run/events.jsonl"
			lines := bytes.Split(bytes.TrimSpace(files[journal]), []byte{'\n'})
			var event agent.Event
			_ = json.Unmarshal(lines[1], &event)
			receipt, _ := json.Marshal(map[string]any{"key": "release-android", "status": "succeeded", "nodes": checkpoint.Nodes})
			event.Message.Content[0].Content, _ = json.Marshal(string(receipt))
			lines[1], _ = json.Marshal(event)
			files[journal] = bytes.Join(lines, []byte{'\n'})
			failures := validateReleaseAuditGraph("audit-run", "android", files)
			if !slices.ContainsFunc(failures, func(failure string) bool { return strings.Contains(failure, wanted) }) {
				t.Fatalf("invalid execution order was not detected: %v", failures)
			}
		})
	}
}
