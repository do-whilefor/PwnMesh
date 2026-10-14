//go:build linux

package integration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/dispatcher"
)

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

// This workload corrects an interpretation of immutable input. It does not
// invalidate a truthful observation merely because a mutable target changed.
func TestLiveCurationRelationsProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_CURATION_RELATIONS_TEST") != "1" {
		t.Skip("opt in to the real model and Docker curation-relations acceptance")
	}
	origin := fmt.Sprintf(`Exercise observation correction using ONLY synthetic local files under /workspace. Do not read credentials, install packages, or contact other services.
Authorize exactly one ordinary Step. Its Worker must first write these exact bytes to %s: %q, and these exact initial unverified transcription bytes to %s: %q. Publish the mistaken interpretation as a fact with exactly this description: %q, scope sample-v1, and evidence from the transcription file. This is an intentionally wrong interpretation of the dataset, not a claim that a historical event never happened.
Next, in the SAME Step, use a deterministic bash command to read every integer from the retained values file, calculate the sum, and compute SHA-256 of its exact bytes. Save AND print a JSON receipt to %s with fields sum (computed, never hardcoded) and dataset_sha256. Publish a second fact with exactly this description: %q, scope sample-v1, and evidence from BOTH the unchanged values file and the calculation receipt. Finish the Step with the second fact as its result. Preserve all three files. No candidate, dispute, report, or additional Step is needed.
The blackboard Agent must submit one curate action with groups:[] and relations:[{kind:"supersedes",source:the second fact ID,target:the first fact ID,reason:the calculation corrected the initial interpretation}]. Never rewrite or delete the original fact or evidence. The main Agent may complete only after this relation has been curated, citing the effective recalculated fact.`, curationValuesPath, curationValues, curationOriginalPath, curationOriginal, curationOldClaim, curationCorrectedPath, curationNewClaim)
	runObservedProject(t, "Correct an observation while retaining original evidence", origin, "Correct the initial sum=11 interpretation with a reproducible sum=10 calculation, atomically curate the supersedes relation, retain all original evidence, and complete using only effective support.", "atomic_curation_relation_with_retained_evidence", validateCurationRelationsDelivery, 1)
}

// The original interpretation is tentative, not an opposing verdict that
// would require independent review. Curation must retain its provenance while
// excluding its superseded premises from the final supported conclusion.
func TestLiveCurationSupportProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_CURATION_SUPPORT_TEST") != "1" {
		t.Skip("opt in to the real model and Docker current-support acceptance")
	}
	origin := fmt.Sprintf(`Exercise current-support curation using ONLY synthetic local files under /workspace. Do not read credentials, install packages, or contact other services.
Authorize exactly one ordinary Step. Its Worker first writes these exact bytes to %s: %q, and this intentionally wrong initial transcription to %s: %q. Publish a fact with exactly this description: %q, scope sample-v1, and evidence from the transcription file. Then publish a candidate with claim %q, scope sample-v1, status candidate (tentative, never refuted or verified), sources containing only that initial fact ID, and explicit evidence from the transcription file. Explain that the unverified sum=11 transcription does not yet establish the common claim.
In the SAME Step, use a deterministic bash command to read every integer from the retained values file, calculate the sum, and compute SHA-256 of its exact bytes. Save AND print a JSON receipt to %s with fields sum (computed, never hardcoded) and dataset_sha256. Publish a fresh fact with exactly this description: %q, scope sample-v1, and evidence from BOTH the unchanged input and the calculation receipt. Publish a second candidate with the EXACT SAME claim %q and scope sample-v1, status verified, sources containing only the fresh fact ID, and explicit evidence from BOTH input and receipt. Finish the Step with the fresh fact as its result. Preserve all original files, facts and candidates.
The blackboard Agent must atomically submit the supersedes relation from the fresh fact to the initial fact and one group containing BOTH candidate IDs, status verified, and a reason explaining the new calculation and superseded premise. Keep both historical CandidateIDs. The final Finding's current Sources and Evidence must contain only the valid fresh calculation support. If an early curation ran before both candidates were available, its later update must preserve the same final history and effective support. This is tentative-to-verified correction, with no opposing verified/refuted verdict, so no dispute or independent-review Step is needed. The main Agent may complete only after the supported verified Finding and relation are curated, citing the fresh fact. No report or extra Step is needed.`, curationValuesPath, curationValues, curationOriginalPath, curationOriginal, curationOldClaim, curationSupportClaim, curationCorrectedPath, curationNewClaim, curationSupportClaim)
	runObservedProject(t, "Retain historical candidates with fresh verified support", origin, "Recalculate 2+3+5 from retained bytes, supersede the mistaken interpretation, retain both candidate judgments, and complete with a verified Finding supported only by the fresh calculation.", "current_curation_support_with_retained_candidates", validateCurationSupportDelivery, 1)
}

func TestLiveEfficiencyProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_EFFICIENCY_TEST") != "1" {
		t.Skip("opt in to the local audit fixture and real model")
	}
	runLiveEfficiencyProject(t, false)
}

func TestLiveEfficiencyOrchestrationProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_EFFICIENCY_ORCHESTRATION_TEST") != "1" {
		t.Skip("opt in to the multi-worker local audit and real model")
	}
	runLiveEfficiencyProject(t, true)
}

// This test runs the production Server, Scheduler, Docker bridge and Worker.
// The optional proxy only observes upstream bytes; every model response is real.
// Direct mode preserves the configured hostname for provider-specific behavior.
// Local fixture data is the only task content sent to the model service.
func TestLiveContentionProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_CONTENTION_TEST") != "1" {
		t.Skip("opt in with model configuration, PWNMESH_DOCKER_TEST_IMAGE and PWNMESH_LIVE_OUTPUT")
	}
	origin, goal := liveContentionTask()
	runObservedProject(t, "Live concurrent transaction audit", origin, goal, "business_acceptance", validateLiveContention)
}

// A synthetic disagreement, not an audit of a remote service. The two
// supplied reports intentionally disagree about the same immutable dataset.
// Only the independent review reads the actual data and recomputes the sum.
func TestLiveOrchestrationProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_ORCHESTRATION_TEST") != "1" {
		t.Skip("opt in to the real model and Docker orchestration acceptance")
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/report-a":
			_, _ = w.Write([]byte(`{"dataset":"sample-v1","reported_sum":10,"report":"a"}`))
		case "/report-b":
			_, _ = w.Write([]byte(`{"dataset":"sample-v1","reported_sum":11,"report":"b"}`))
		case "/dataset":
			_, _ = w.Write([]byte(`{"dataset":"sample-v1","values":[2,3,5]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()
	origin := fmt.Sprintf(`Exercise the orchestration protocol on three synthetic immutable local JSON resources at %s. Scope is ONLY those URLs and /workspace. Do not read credentials, install packages, or contact other services.
The supplied reports intentionally disagree; preserve both, then independently verify the real data. Use exactly this claim: "The sum of sample-v1 values is 10" and scope: "sample-v1" for all candidate judgments.
First the main Agent should authorize two separate independent ordinary Steps in one plan, both with no depends_on: one Worker retrieves ONLY /report-a, one retrieves ONLY /report-b. Each writes the original JSON HTTP response body to its own evidence file, publishes a fact accurately describing that report (the report is not ground truth), and submits a candidate for the common claim: verified for report-a's assertion, refuted for report-b's assertion. These are deliberately conflicting candidate interpretations, not final verified Findings. Initial workers must not inspect /dataset or resolve the disagreement. Each task finishes after publishing its report and candidate.
The blackboard Agent preserves both sources, marks the conflict, and asks for independent review. Once a dispute exists and the first two tasks have succeeded, the main Agent creates exactly one new Step with its dispute_id AND depends_on containing exactly the two initial Step IDs. This is a structured successful-completion dependency, not just prose or from fact IDs. Ask an independent Worker to examine both original reports and GET /dataset, save its original JSON response body, and calculate the sum with a deterministic command that reads that saved body. The command must save AND print a JSON calculation receipt with these fields: dataset (from the input), values (the actual input array), sum (computed, not hardcoded), dataset_sha256 (SHA-256 of the exact saved body bytes). Retain this receipt as a separate evidence file. File names are unrestricted; cite both the original dataset body and calculation receipt as evidence in the review fact and candidate. The reviewer must publish a fact containing the dataset and independently calculated sum, and a candidate for the common claim with new review evidence. It must not merely repeat either report.
The blackboard Agent then resolves only with the independent review fact IDs, preserving original candidates. If evidence is insufficient it must keep the dispute uncertain. The main Agent may complete only after the dispute is resolved and all observations are curated. No additional report-writing Step is needed: the persisted Findings, dispute history, and absolute /workspace evidence references are the delivery.`, fixture.URL)
	goal := "Retain both conflicting report candidates, open a dispute, authorize one independent review in a new run, verify that 2+3+5=10 from the dataset, resolve with that evidence, and complete only after curation."
	runObservedProject(t, "Synthetic independent-review orchestration", origin, goal, "independent_review_with_retained_evidence", validateOrchestrationDelivery, 1)
}

func TestLiveModelReadiness(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_READINESS_TEST") != "1" {
		t.Skip("opt in with PWNMESH_LIVE_READINESS_TEST=1, model environment and PWNMESH_LIVE_OUTPUT")
	}
	base, token, model := os.Getenv("ANTHROPIC_BASE_URL"), os.Getenv("ANTHROPIC_AUTH_TOKEN"), os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model = os.Getenv("ANTHROPIC_DEFAULT_FABLE_MODEL")
	}
	output := os.Getenv("PWNMESH_LIVE_OUTPUT")
	if strings.TrimSpace(base) == "" || strings.TrimSpace(token) == "" || strings.TrimSpace(model) == "" || strings.TrimSpace(output) == "" {
		t.Fatal("explicit model endpoint, authentication, model selection and PWNMESH_LIVE_OUTPUT are required")
	}
	upstream, err := url.Parse(base)
	if err != nil || upstream.Host == "" || (upstream.Scheme != "https" && upstream.Scheme != "http") || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
		t.Fatal("model endpoint must be an HTTP(S) URL without user info, query or fragment")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal("cannot create readiness evidence directory")
	}
	env := map[string]string{
		"ANTHROPIC_BASE_URL": base, "ANTHROPIC_AUTH_TOKEN": token, "ANTHROPIC_MODEL": model,
		"PWNMESH_REASONING_EFFORT": "max",
	}
	for _, key := range []string{"PWNMESH_MAX_OUTPUT_TOKENS", "PWNMESH_REQUEST_TIMEOUT"} {
		if value := config.Getenv(key); value != "" {
			env[key] = value
		}
	}
	c := config.Config{
		Runtime: config.Runtime{MaxWorkers: 1, HealthMode: "startup_only", HealthTimeout: 120},
		Tasks:   config.Tasks{Reason: config.Task{ReasoningEffort: "max"}},
		Workers: []config.Worker{{Name: "live-readiness", Type: "go", TaskTypes: []string{"reason"}, MaxRunning: 1, Env: env}},
	}
	scheduler := dispatcher.New(c, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	started := time.Now().UTC()
	checkErr := scheduler.Health(ctx, true)
	evidence := readinessLiveResult(model, upstream.Scheme+"://"+upstream.Host, started, time.Now().UTC(), checkErr)
	path := filepath.Join(output, "readiness.json")
	if err := saveLiveJSON(path, evidence); err != nil {
		t.Fatal("cannot save readiness evidence")
	}
	t.Logf("readiness_passed=%t wall_ms=%d failure_kind=%s evidence=%s", evidence.Passed, evidence.WallMS, evidence.FailureKind, path)
	if checkErr != nil {
		t.Fatal("production readiness check failed; inspect the credential-free readiness.json evidence")
	}
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

func TestLiveReleaseAuditReuseProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_RELEASE_AUDIT_REUSE_TEST") != "1" {
		t.Skip("opt in to real model verified graph reuse after a deterministic join failure")
	}
	fixture, observations := newReleaseAuditReuseHTTP(t)
	origin := fmt.Sprintf(`Audit synthetic Android and Linux client releases at %s using two independent platform Steps and a separate delivery Step whose structured depends_on names both successful platform Steps. Only this local HTTP origin and /workspace are authorized; do not contact manifest endpoints, read credentials, install software, or execute downloaded files. Python stdlib is sufficient. These are inert ZIP fixtures, not runnable APKs or binaries. Preserve this acceptance contract in delegated tasks.
Declare the immutable Step write_paths exactly as follows: Android ["/workspace/release-audit/android"], Linux ["/workspace/release-audit/linux"], delivery ["/workspace/release-audit/report.json"]. The platform Steps have no depends_on; delivery depends_on names both platform Step IDs. Keep branch outputs private and give delivery sole ownership of the final report; do not declare the common /workspace/release-audit parent.
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
			failures = append(failures, validateReleaseAuditReuseWrites(state, files)...)
			if err := saveLiveJSON(filepath.Join(os.Getenv("PWNMESH_LIVE_OUTPUT"), "release-http-requests.json"), requests); err != nil {
				failures = append(failures, "cannot retain fixture HTTP observations: "+err.Error())
			}
			return failures
		}, 1)
}

// Two independent producers exercise scenario guidance, evidence retention,
// candidate publication, concurrent scheduling and final curation together.
func TestLiveScenarioSkillsReviewProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_SCENARIO_REVIEW_TEST") != "1" {
		t.Skip("opt in to real model scenario Skills and multi-producer curation")
	}
	for _, scenario := range []string{"audit", "pentest"} {
		t.Run(scenario, func(t *testing.T) {
			output := os.Getenv("PWNMESH_LIVE_OUTPUT")
			if output == "" {
				t.Fatal("PWNMESH_LIVE_OUTPUT is required")
			}
			t.Setenv("PWNMESH_LIVE_OUTPUT", filepath.Join(output, scenario))
			fixture, server := newScenarioSkillFixture()
			defer server.Close()
			if err := os.MkdirAll(filepath.Join(output, scenario), 0700); err != nil {
				t.Fatal(err)
			}
			if err := saveLiveJSON(filepath.Join(output, scenario, "fixture-oracle.json"), scenarioReviewOracle{fixture.source, fixture.nonce, fixture.bodies}); err != nil {
				t.Fatal(err)
			}
			origin := fmt.Sprintf(`Use only synthetic local resources at %s and Step-private directories under /workspace. Do not install packages, read credentials or contact other targets.
Authorize exactly two independent ordinary Steps in one initial decision, both from origin and with no depends_on. Each Step must independently perform the entire ownership check below, using its OWN directory and freshly fetched evidence. Never reuse the other producer's files or result. This is two reproductions of the same bounded claim; do not split target and control across Steps.
Each Worker publishes one evidence-backed fact containing its complete target/control observations, then one candidate with claim exactly %q, scope exactly %q, status verified if its observations support the claim, sources containing ONLY its own fact ID, and explicit evidence covering ALL files from that fact. Finish using that existing fact ID. No extra candidate or report-writing Step is needed.
The blackboard Agent merges BOTH candidates into one supported verified Finding while retaining both candidate IDs, both independent source facts, and their original evidence. If early curation sees only one candidate, update the same finding once the second arrives. The main Agent completes only after both Steps succeed, both candidates are curated, and the Finding retains both sources. Cite both result fact IDs when completing. Do not complete from the first producer alone.
Required procedure for EACH Step: %s`, server.URL, scenarioReviewClaim, scenarioReviewScope, scenarioOwnershipTask(scenario))
			bundle := scenarioSkillBundle(t, scenario)
			runObservedScenarioProject(t, "Independent "+scenario+" ownership reproductions", origin,
				"Independently reproduce the target/control comparison twice and complete with one curated finding retaining both producers' evidence.",
				scenario, "scenario_skills_multi_producer_curation", func(state board.State, files map[string][]byte) []string {
					fixture.mu.Lock()
					err := saveLiveJSON(filepath.Join(output, scenario, "fixture-observations.json"), map[string]any{"statuses": fixture.seen, "requests": fixture.reads})
					fixture.mu.Unlock()
					if err != nil {
						t.Error(err)
					}
					return validateScenarioReview(state, files, scenario, bundle, fixture)
				}, 1)
		})
	}
}

// Run each scenario separately so its durable archive and database are isolated.
// The task never mentions the Skills: discovery must come from production prompts.
func TestLiveScenarioSkillsProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_SCENARIO_SKILLS_TEST") != "1" {
		t.Skip("opt in to real model and Docker scenario Skills acceptance")
	}
	for _, scenario := range []string{"audit", "pentest"} {
		t.Run(scenario, func(t *testing.T) {
			output := os.Getenv("PWNMESH_LIVE_OUTPUT")
			if output == "" {
				t.Fatal("PWNMESH_LIVE_OUTPUT is required")
			}
			t.Setenv("PWNMESH_LIVE_OUTPUT", filepath.Join(output, scenario))
			fixture, server := newScenarioSkillFixture()
			defer server.Close()
			origin := fmt.Sprintf("Use only synthetic local resources at %s and a Step-private directory under /workspace. Do not install packages, access credentials, or contact other targets. Authorize exactly one ordinary Step. Publish one evidence-backed fact and finish using that fact; no candidates, curation, extra report-writing Step or scoring are needed. ", server.URL)
			origin += scenarioOwnershipTask(scenario)
			bundle := scenarioSkillBundle(t, scenario)
			runObservedScenarioProject(t, "Synthetic "+scenario+" Skills smoke", origin, "Complete the bounded ownership check with original target and control evidence.", scenario, "scenario_skills_and_ownership_controls", func(state board.State, files map[string][]byte) []string {
				fixture.mu.Lock()
				err := saveLiveJSON(filepath.Join(output, scenario, "fixture-observations.json"), fixture.seen)
				fixture.mu.Unlock()
				if err != nil {
					t.Error(err)
				}
				return validateScenarioSkills(state, files, scenario, bundle, fixture)
			}, 1)
		})
	}
}

// Real planning and preparation exercise the production Go Loop. The second
// Step targets a retained historical version, after the preparation Worker has
// already fixed it. Its deterministic check must finish without any model call.
func TestLiveStaleRepairProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_STALE_REPAIR_TEST") != "1" {
		t.Skip("opt in to the real model and Docker stale-repair acceptance")
	}
	originalHash := fmt.Sprintf("%x", sha256.Sum256([]byte(staleRepairOriginal)))
	origin := fmt.Sprintf(`Exercise stale repair on synthetic local JSON files. Scope is ONLY /workspace. Do not read credentials, install packages, or contact other services.
Authorize exactly two ordinary Steps in sequence. First authorize one preparation Worker with no dependencies. It must write these exact UTF-8 bytes, including the final newline, to BOTH %s and %s: %q. It must then replace ONLY %s with these exact bytes: %q. Preserve the historical original file unchanged. Confirm both SHA-256 values using a deterministic command, then publish a fact with both original and corrected file evidence and finish successfully. This ordinary preparation task must have NO repair field.
After that Step has successfully completed, the main Agent must authorize exactly one new repair Step with depends_on containing the preparation Step ID and this structured repair field: {"path":%q,"sha256":%q,"rules":[{"kind":"json_equals","pointer":"/ok","value":true}]}. Its description should ask to fix /ok to true only if repair is still needed. The expected SHA-256 is the actual retained historical bad version, intentionally different from the already-fixed current version. Do not change this expected hash to the current hash and do not omit the Step just because preparation already fixed it: the purpose is to verify deterministic preflight acceptance of obsolete repair work.
The runtime must independently check the target and finish that repair Step as a no-op, retaining the current content, SHA-256, and check receipt without a model session. The main Agent should use the resulting persisted fact, allow observations to be curated, and complete the project. No candidate conflicts, unrelated Steps, retries, or report-writing Steps are needed. The persisted facts, dependency, original/current bytes, and repair receipt are the delivery.`, staleRepairPath, staleRepairOriginalPath, staleRepairOriginal, staleRepairPath, staleRepairFixed, staleRepairPath, originalHash)
	runObservedProject(t, "Stale repair skips already-correct local JSON", origin, "Retain an actual historical bad JSON version; independently accept the already-fixed target with SHA-256 and /ok=true checks; complete the obsolete repair without a model call.", "stale_repair_noop_with_retained_evidence", validateStaleRepairDelivery, 1)
}

// This workload exercises the production Loop, command DAG, graph bridge,
// evidence retention and completion using only synthetic local data in a small image.
func TestLiveWorkerCommandGraphProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_WORKER_GRAPH_TEST") != "1" {
		t.Skip("opt in to real model and Docker command-graph acceptance")
	}
	origin := `Exercise a small Worker command graph using ONLY synthetic local files under /workspace. Do not read credentials, install packages or contact other services.
The main Agent must authorize exactly one ordinary Step and preserve the following execution contract in its task. The Worker must use the run_graph tool with key sample-sum and parallelism 2, with exactly three required nodes: left, right, sum. left and right have no dependencies. left executes sleep 1 followed by printing the JSON array [2,3] to stdout; right executes sleep 1 followed by printing [5]. These deliberate one-second waits make actual branch overlap observable. sum depends_on both left and right, reads the two retained stdout files through the PWNMESH_DEPENDENCIES JSON file, concatenates their arrays in left/right order, and computes their sum with Python. It prints exactly one JSON object with values (the actual concatenated array) and sum (computed, never hardcoded). Use the tool's private node directories and retained stdout.log files; no shared output file is necessary. Do not simulate the graph with a bash background job, extra Steps, or sequential tool calls.
After the graph succeeds, publish one fact with scope sample-v1 describing the independently executed branches and calculated sum, using all three absolute stdout paths as evidence. Return that fact as the Step result. There is no ambiguous interpretation, report or dispute to create. The main Agent may complete after the Step succeeds, using its accepted result and retained raw evidence. No additional Step, curation or candidate judgment is needed.`
	runObservedProject(t, "Worker command DAG with parallel inputs and deterministic join", origin,
		"Execute the required three-node Worker graph, retain both input arrays and the computed values=[2,3,5], sum=10 receipt, and complete from the accepted evidence without curation.",
		"worker_command_graph_with_retained_evidence", validateWorkerCommandGraphDelivery, 1)
}

func TestLiveWorkerMixedGraphProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_WORKER_MIXED_GRAPH_TEST") != "1" {
		t.Skip("opt in to real model and Docker dynamic multi-Agent graph acceptance")
	}
	origin := `Use only synthetic local files under /workspace. Do not read credentials, install packages or contact other services.
Authorize exactly one ordinary Step preserving this execution contract. The parent Worker is the scheduling Agent: use its existing Loop to observe results and choose subsequent Agent assignments, node IDs and dependencies. Do not predeclare the whole graph or simulate Agents with shell background jobs.
The first successful run_graph call, key sample-agents, contains only one required discovery node of kind agent, with resources:[] and no dependencies. Its task is to use local Python tools and random.SystemRandom to generate 3 to 5 categories with distinct random names and 3 to 6 integer values per category, write manifest.json as {"categories":[{"name":"generated-name","values":[integers]}]}, and declare that file as an artifact. The discovery Agent returns the actual manifest JSON as its final answer so the parent sees the discovered categories in the tool receipt. Neither category names nor their count may be prescribed before discovery runs.
Only AFTER receiving that discovery result, choose one new Agent task for each actual category and append these nodes under the SAME graph key. Each category Agent depends on discovery, reads the original declared manifest artifact through PWNMESH_DEPENDENCIES (an absolute JSON FILE PATH), computes its assigned category from those values using local tools, and writes a declared result.json containing {"category":name,"count":number-of-values,"sum":computed-sum}. Node IDs are your choice; use resources:[] and private node output files. Child Agents return local accounts and do not publish blackboard records.
Append one required command join, in the same or a later call, depending on every category Agent. It reads their declared result.json artifacts through PWNMESH_DEPENDENCIES, and prints exactly one JSON object {"categories":[the actual per-category result objects],"count":computed-total-count,"sum":computed-total-sum}. Do not hardcode results. Every run_graph call with this key supplies the FULL cumulative node list: retain each old node definition unchanged and add new nodes; never delete or edit old nodes. The tool accepts 1..64 total nodes and parallelism 1..16; omitted parallelism defaults to min(node-count,16). Leave parallelism omitted for this exercise. Retained nodes must not execute again.
After the graph succeeds, the parent finishes the Step with one evidence-backed fact. Its fact.scope field must be exactly the string "mixed-agent-v2", with no explanation or extra text; put explanations in fact.description. Preserve this exact field requirement in the delegated Step. Evidence must include the original manifest.json, every result.json, and the join stdout.log. Child final prose is not raw evidence. The main Agent completes from the accepted Step result; no additional review, report or curation is needed. This tests data-driven Agent scheduling, not an arithmetic performance claim.`
	runObservedProject(t, "Worker Agent discovers work and dynamically delegates by category", origin,
		"Discover unpredictable local categories, then extend one Worker graph with an Agent for each observed category and a computed join; preserve original evidence and complete the project.",
		"worker_dynamic_mixed_graph_with_isolated_loops", validateWorkerMixedGraphDelivery, 1)
}
