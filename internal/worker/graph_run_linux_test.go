//go:build linux

package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/board"
	"pwnmesh/internal/workergraph"
)

func graphOutputResults(t *testing.T, raw []byte) []Result {
	t.Helper()
	var results []Result
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for {
		var value Result
		if err := decoder.Decode(&value); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("invalid output stream: %v", err)
		}
		if value.Type == "result" {
			results = append(results, value)
		}
	}
	return results
}

func graphWrapperJob(t *testing.T) Job {
	j := outcomeJob(t, "explore")
	j.Graph.Project.OrchestrationVersion = 1
	return j
}

func graphSessionFixture(ctx context.Context, j Job, o Options, result Result) (Result, error) {
	var err error
	result, err = prepareFinalEvidence(ctx, j, o.RunDir, result, nil)
	if err != nil {
		return Result{}, err
	}
	return graphRawSessionFixture(o, result)
}

func graphRawSessionFixture(o Options, result Result) (Result, error) {
	if err := os.WriteFile(filepath.Join(o.RunDir, "session.json"), []byte(`{"durable":true}`), 0600); err != nil {
		return Result{}, err
	}
	return result, json.NewEncoder(o.Output).Encode(result)
}

func TestGraphResultWriterStreamsEventsAndFiltersSplitResults(t *testing.T) {
	var sink bytes.Buffer
	writer := &graphResultWriter{writer: &sink}
	for _, piece := range []string{`{"type":"graph_`, "request", "\",\"request\":{}}\n", `{"type":"result","status":`, "\"success\"}\n", "{\"type\":\"message_end\"}\n"} {
		if _, err := io.WriteString(writer, piece); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(sink.String(), `"type":"graph_request"`) || !strings.Contains(sink.String(), `"type":"message_end"`) || len(graphOutputResults(t, sink.Bytes())) != 0 {
		t.Fatalf("events held or result leaked: %s", &sink)
	}
	var wg sync.WaitGroup
	for n := 0; n < 20; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, err := fmt.Fprintf(writer, "{\"type\":\"tool_end\",\"index\":%d}\n", n); err != nil {
				t.Error(err)
			}
		}(n)
	}
	wg.Wait()
	if err := writer.finish(Result{Type: "result", Status: "failed", FailureKind: "graph_checkpoint"}); err != nil {
		t.Fatal(err)
	}
	results := graphOutputResults(t, sink.Bytes())
	if len(results) != 1 || results[0].Status != "failed" || strings.Count(sink.String(), `"type":"tool_end"`) != 20 {
		t.Fatalf("bad final framing: %s", &sink)
	}
	if err := writer.finish(Result{Type: "result", Status: "success"}); err == nil {
		t.Fatal("duplicate final result permitted")
	}
}

func TestGraphWrapperVerificationFailurePublishesOnlyFailedResult(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	target := filepath.Join(j.Workspace, "report.txt")
	if err := os.WriteFile(target, []byte("already good"), 0600); err != nil {
		t.Fatal(err)
	}
	j.Repair = &artifactcheck.Spec{Path: target, SHA256: strings.Repeat("a", 64), Rules: []artifactcheck.Rule{{Kind: "text_contains", Text: "good"}}}
	var output bytes.Buffer
	sessionRun := func(ctx context.Context, job Job, o Options) (Result, error) {
		// The session emits success, but it lacks the matching repair receipt.
		// The graph verifier must reject it before any result reaches Docker.
		return graphSessionFixture(ctx, job, o, Result{Type: "result", Status: "success", Text: completedOutput("explore")})
	}
	r, err := runWorkerGraph(context.Background(), j, Options{RunDir: dir, Output: &output}, sessionRun)
	results := graphOutputResults(t, output.Bytes())
	if err != nil || r.Status != "failed" || r.FailureKind != "graph_checkpoint" || !strings.Contains(r.Error, "repair target changed") || len(results) != 1 || results[0].Status != "failed" {
		t.Fatalf("unaccepted success leaked: %+v %v output=%s", r, err, &output)
	}
}

func TestGraphWrapperBridgeRequestsRemainLive(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	var output bytes.Buffer
	requests := 0
	bridge := &draftTestBridge{dir: dir, handle: func(GraphRequest) (any, error) { requests++; return map[string]bool{"ready": true}, nil }}
	stream := io.MultiWriter(&output, bridge)
	sessionRun := func(ctx context.Context, job Job, o Options) (Result, error) {
		if _, err := io.WriteString(o.Output, "{\"type\":\"message_start\"}\n"); err != nil {
			return Result{}, err
		}
		reply, err := graphRPC(ctx, dir, o.Output, GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_graph", Section: "facts"})
		if err != nil {
			return Result{}, err
		}
		if requests != 1 || reply != `{"ready":true}` || !strings.Contains(output.String(), `"type":"message_start"`) {
			return Result{}, errors.New("live bridge or events were buffered")
		}
		return graphSessionFixture(ctx, job, o, Result{Type: "result", Status: "success", Text: completedOutput("explore")})
	}
	r, err := runWorkerGraph(context.Background(), j, Options{RunDir: dir, Output: stream}, sessionRun)
	if err != nil || r.Status != "success" || len(graphOutputResults(t, output.Bytes())) != 1 {
		t.Fatalf("bridge failed: %+v %v %s", r, err, &output)
	}
}

func TestGraphWrapperActualLoopAcceptsAndReplaysOneResult(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	var output bytes.Buffer
	calls := 0
	opts := Options{RunDir: dir, Output: &output, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		calls++
		return agent.Text("assistant", completedOutput("explore")), nil
	})}
	first, err := runTestWorker(context.Background(), j, opts)
	if err != nil || first.Status != "success" || len(graphOutputResults(t, output.Bytes())) != 1 {
		t.Fatalf("first run: %+v %v %s", first, err, &output)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "graph", "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint workergraph.Checkpoint
	if err = json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatal(err)
	}
	accepted := false
	for _, node := range checkpoint.Nodes {
		if node.ID == "accept" {
			accepted = node.Status == "succeeded" && len(node.Output.Value) > 0 && len(node.Output.Artifacts) > 0
		}
	}
	if checkpoint.Status != "succeeded" || !accepted {
		t.Fatalf("acceptance was not durable: %+v", checkpoint)
	}
	output.Reset()
	second, err := runTestWorker(context.Background(), j, opts)
	results := graphOutputResults(t, output.Bytes())
	if err != nil || second.Status != "success" || calls != 1 || len(results) != 1 || results[0].Text != first.Text {
		t.Fatalf("cache replay: %+v %v calls=%d %s", second, err, calls, &output)
	}
}

func TestGraphWrapperCachedResultHonorsHardStopAndLaunchToken(t *testing.T) {
	for _, test := range []string{"cancelled", "launch"} {
		t.Run(test, func(t *testing.T) {
			j, dir := graphWrapperJob(t), t.TempDir()
			var output bytes.Buffer
			calls := 0
			opts := Options{RunDir: dir, Output: &output, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
				calls++
				return agent.Text("assistant", completedOutput("explore")), nil
			})}
			if _, err := runTestWorker(context.Background(), j, opts); err != nil {
				t.Fatal(err)
			}
			if test == "cancelled" {
				if err := os.WriteFile(filepath.Join(dir, "cancelled"), []byte("hard stop\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv("PWNMESH_LAUNCH_TOKEN", strings.Repeat("a", 32))
				if err := os.WriteFile(filepath.Join(dir, "launch-token"), []byte(strings.Repeat("b", 32)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			output.Reset()
			result, err := runTestWorker(context.Background(), j, opts)
			if err == nil || test == "cancelled" && !errors.Is(err, context.Canceled) || calls != 1 || result.Status == "success" || len(graphOutputResults(t, output.Bytes())) != 0 {
				t.Fatalf("cached result bypassed %s: %+v %v %s", test, result, err, &output)
			}
		})
	}
}

func TestGraphWrapperRetryableResultIsEmittedOnceAndReconciled(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	var output bytes.Buffer
	calls := 0
	sessionRun := func(ctx context.Context, job Job, o Options) (Result, error) {
		calls++
		result := Result{Type: "result", Status: "success", Text: completedOutput("explore")}
		if calls == 1 {
			result = Result{Type: "result", Status: "failed", Retryable: true, FailureKind: "transport", Error: "recoverable connection reset"}
		}
		return graphSessionFixture(ctx, job, o, result)
	}
	opts := Options{RunDir: dir, Output: &output}
	first, err := runWorkerGraph(context.Background(), j, opts, sessionRun)
	if err != nil || !first.Retryable || len(graphOutputResults(t, output.Bytes())) != 1 {
		t.Fatalf("retryable terminal framing: %+v %v %s", first, err, &output)
	}
	output.Reset()
	second, err := runWorkerGraph(context.Background(), j, opts, sessionRun)
	if err != nil || second.Status != "success" || calls != 2 || len(graphOutputResults(t, output.Bytes())) != 1 {
		t.Fatalf("graph did not reconcile retryable session: %+v %v %s", second, err, &output)
	}
}

func TestGraphWrapperPublishedFactEvidenceAndCompactedHistory(t *testing.T) {
	for _, test := range []string{"receipt", "compacted", "uncommitted_tail", "bad_journal_hash", "wrong_tool", "wrong_run", "omitted_receipt", "unfrozen_evidence"} {
		t.Run(test, func(t *testing.T) {
			j, dir := graphWrapperJob(t), t.TempDir()
			j.ResultContractVersion, j.GraphRPC = 2, true
			var output bytes.Buffer
			calls, evidencePath := 0, ""
			sessionRun := func(ctx context.Context, job Job, o Options) (Result, error) {
				calls++
				ref, err := freezeEvidenceBytes(ctx, job.RunID, o.RunDir, []byte("original evidence\n"))
				if err != nil {
					return Result{}, err
				}
				evidencePath = ref.Path
				fact := board.FactRecord{ID: "f_published", Status: "valid", RunID: "worker@" + job.RunID, SourceStepID: job.Intent.ID, Evidence: []board.EvidenceRef{ref}}
				if test == "wrong_run" {
					fact.RunID = "other-run"
				}
				if test == "unfrozen_evidence" {
					fact.Evidence[0].Path = filepath.Join(job.Workspace, "mutable.txt")
				}
				factJSON, _ := json.Marshal(fact)
				receipt := board.StateActionResult{Op: "fact", ID: fact.ID, Revision: 1, Result: factJSON}
				if test == "omitted_receipt" {
					receipt.Result = nil
				}
				receiptJSON, _ := json.Marshal(receipt)
				// This double encoding is the actual Loop tool_result wire format.
				content, _ := json.Marshal(string(receiptJSON))
				toolName := "graph_action"
				if test == "wrong_tool" {
					toolName = "bash"
				}
				history := []agent.Message{
					{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "publish", Name: toolName, Input: json.RawMessage(`{"op":"fact","idempotency_key":"published","payload":{}}`)}}},
					{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "publish", Content: content}}},
				}
				result := Result{Type: "result", Status: "success", Text: `{"accepted":true,"outcome":"completed","data":{"fact_id":"f_published"}}`}
				identity, err := identityFor(job, o.RunDir)
				if err != nil {
					return Result{}, err
				}
				saved := session{SchemaVersion: sessionSchemaVersion, Identity: identity, RunID: job.RunID, Kind: job.Kind, StartedAt: time.Now(), Result: &result, History: history}
				journal, err := openJournal(o.RunDir, nil)
				if err != nil {
					return Result{}, err
				}
				defer journal.file.Close()
				for _, message := range history {
					if err = journal.append(agent.Event{Type: "message_end", Message: &message}); err != nil {
						return Result{}, err
					}
				}
				if test == "compacted" || test == "uncommitted_tail" || test == "bad_journal_hash" {
					saved.History = []agent.Message{agent.Text("user", "Runtime context summary: fact f_published was published.")}
				}
				if err = saved.save(o.RunDir, journal); err != nil {
					return Result{}, err
				}
				if test == "bad_journal_hash" {
					saved.Log.SHA256 = strings.Repeat("0", 64)
					raw, _ := json.Marshal(saved)
					if err = os.WriteFile(filepath.Join(o.RunDir, "session.json"), raw, 0600); err != nil {
						return Result{}, err
					}
				}
				if test == "uncommitted_tail" {
					if _, err = journal.file.Write([]byte("uncommitted partial crash tail")); err != nil {
						return Result{}, err
					}
				}
				return result, json.NewEncoder(o.Output).Encode(result)
			}
			opts := Options{RunDir: dir, Output: &output}
			result, err := runWorkerGraph(context.Background(), j, opts, sessionRun)
			wantSuccess := test == "receipt" || test == "compacted" || test == "uncommitted_tail"
			if err != nil || (result.Status == "success") != wantSuccess || len(graphOutputResults(t, output.Bytes())) != 1 {
				t.Fatalf("published evidence binding: %+v %v output=%s", result, err, &output)
			}
			if !wantSuccess {
				if result.FailureKind != "graph_checkpoint" {
					t.Fatalf("wrong failure: %+v", result)
				}
				return
			}
			var checkpoint workergraph.Checkpoint
			raw, err := os.ReadFile(filepath.Join(dir, "graph", "graph.json"))
			if err != nil || json.Unmarshal(raw, &checkpoint) != nil {
				t.Fatal("missing graph checkpoint")
			}
			bound := false
			for _, node := range checkpoint.Nodes {
				if node.ID == "accept" {
					for _, artifact := range node.Output.Artifacts {
						bound = bound || artifact.Path == evidencePath
					}
				}
			}
			if !bound {
				t.Fatal("published fact evidence was not bound to the accepted graph output")
			}
			if err := os.Chmod(evidencePath, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(evidencePath, []byte("tampered evidence\n"), 0600); err != nil {
				t.Fatal(err)
			}
			output.Reset()
			result, err = runWorkerGraph(context.Background(), j, opts, sessionRun)
			if err != nil || result.Status != "failed" || !strings.Contains(result.Error, "SHA-256 changed") || calls != 1 || len(graphOutputResults(t, output.Bytes())) != 1 {
				t.Fatalf("tampered evidence reused: %+v %v calls=%d", result, err, calls)
			}
		})
	}
}

func TestGraphWrapperInlineFactVerifiesRetainedEvidenceBeforeAcceptance(t *testing.T) {
	for _, test := range []string{"valid", "changed_content", "wrong_run", "wrong_excerpt", "wrong_line_range", "unfrozen_path", "conflicting_selection"} {
		t.Run(test, func(t *testing.T) {
			j, dir := graphWrapperJob(t), t.TempDir()
			j.ResultContractVersion, j.GraphRPC = 2, true
			var output bytes.Buffer
			calls := 0
			sessionRun := func(ctx context.Context, job Job, o Options) (Result, error) {
				calls++
				ref, err := freezeEvidenceBytes(ctx, job.RunID, o.RunDir, []byte("original evidence\n"))
				if err != nil {
					return Result{}, err
				}
				refs := []board.EvidenceRef{ref}
				switch test {
				case "changed_content":
					// Simulate corruption after session success but before graph
					// acceptance. Rehashing these new bytes must not certify them.
					if err := os.Chmod(ref.Path, 0600); err != nil {
						return Result{}, err
					}
					if err := os.WriteFile(ref.Path, []byte("replaced evidence\n"), 0600); err != nil {
						return Result{}, err
					}
				case "wrong_run":
					refs[0].RunID = "other-run"
				case "wrong_excerpt":
					refs[0].Excerpt = "unsupported observation"
				case "wrong_line_range":
					refs[0].StartLine, refs[0].EndLine = 2, 2
				case "unfrozen_path":
					refs[0].Path = filepath.Join(job.Workspace, "mutable-evidence.txt")
					if err := os.WriteFile(refs[0].Path, []byte(ref.Excerpt), 0600); err != nil {
						return Result{}, err
					}
				case "conflicting_selection":
					refs = append(refs, ref)
					refs[1].Excerpt = "unsupported second selection"
				}
				raw, err := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": map[string]any{
					"description": "Observed the original evidence", "scope": "one fixture", "observed_at": "2026-09-28T10:00:00Z", "evidence": refs,
				}}})
				if err != nil {
					return Result{}, err
				}
				return graphRawSessionFixture(o, Result{Type: "result", Status: "success", Text: string(raw)})
			}
			opts := Options{RunDir: dir, Output: &output}
			result, err := runWorkerGraph(context.Background(), j, opts, sessionRun)
			results := graphOutputResults(t, output.Bytes())
			if err != nil || len(results) != 1 || (result.Status == "success") != (test == "valid") {
				t.Fatalf("inline evidence acceptance: %+v %v output=%s", result, err, &output)
			}
			if test != "valid" {
				if result.FailureKind != "graph_checkpoint" || !strings.Contains(result.Error, "evidence") {
					t.Fatalf("invalid snapshot did not fail graph acceptance: %+v", result)
				}
				return
			}
			output.Reset()
			again, err := runWorkerGraph(context.Background(), j, opts, sessionRun)
			if err != nil || again.Status != "success" || again.Text != result.Text || calls != 1 || len(graphOutputResults(t, output.Bytes())) != 1 {
				t.Fatalf("valid inline evidence was not reused: %+v %v calls=%d", again, err, calls)
			}
		})
	}
}

func TestGraphWrapperActualLoopRejectsPreviouslyRehashedInlineSnapshot(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	j.ResultContractVersion = 2
	source := filepath.Join(j.Workspace, "response.txt")
	if err := os.WriteFile(source, []byte("original evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	calls := 0
	opts := Options{RunDir: dir, Output: &output, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		calls++
		return agent.Text("assistant", finalEvidenceOutput(source)), nil
	})}
	first, err := runTestWorker(context.Background(), j, opts)
	if err != nil || first.Status != "success" {
		t.Fatalf("real Loop inline acceptance: %+v %v", first, err)
	}
	refs := finalEvidenceRefs(t, j, first)
	output.Reset()
	again, err := runTestWorker(context.Background(), j, opts)
	if err != nil || again.Status != "success" || again.Text != first.Text || calls != 1 {
		t.Fatalf("real Loop inline replay: %+v %v calls=%d", again, err, calls)
	}
	if err := os.Chmod(refs[0].Path, 0600); err != nil {
		t.Fatal(err)
	}
	replaced := []byte("replaced evidence\n")
	if err := os.WriteFile(refs[0].Path, replaced, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "graph", "graph.json")
	raw, err := os.ReadFile(path)
	var checkpoint workergraph.Checkpoint
	if err != nil || json.Unmarshal(raw, &checkpoint) != nil {
		t.Fatal("cannot read accepted graph")
	}
	// Model a checkpoint written by the former path-only acceptance: the
	// agent certified replacement bytes, then crashed before the accept node.
	checkpoint.Status = "running"
	for n := range checkpoint.Nodes {
		node := &checkpoint.Nodes[n]
		if node.ID == "accept" {
			*node = workergraph.NodeState{ID: "accept", Kind: "function", Status: "pending"}
		}
		if node.ID == "agent" {
			for i := range node.Output.Artifacts {
				if node.Output.Artifacts[i].Path == refs[0].Path {
					node.Output.Artifacts[i].SHA256 = fmt.Sprintf("%x", sha256.Sum256(replaced))
				}
			}
		}
	}
	raw, err = json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	recovered, err := runTestWorker(context.Background(), j, opts)
	if err != nil || recovered.Status != "failed" || recovered.FailureKind != "graph_checkpoint" || !strings.Contains(recovered.Error, "content hash or excerpt") || calls != 1 || len(graphOutputResults(t, output.Bytes())) != 1 {
		t.Fatalf("previously rehashed snapshot reused: %+v %v calls=%d", recovered, err, calls)
	}
}
