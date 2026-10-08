//go:build linux

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/docker"
	"pwnmesh/internal/worker"
)

// The only credential is generated in the Dispatcher-side test process. Tool
// inputs contain its digest, never its value. Exercise the real Worker, file
// and shell tools, concurrent child Agents, model bridge and durable handoff.
func TestDockerModelCredentialsStayOutsideWorker(t *testing.T) {
	image := os.Getenv("PWNMESH_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set PWNMESH_DOCKER_TEST_IMAGE for real model credential isolation acceptance")
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	token := "audit-" + hex.EncodeToString(random) + "-token"
	digest := sha256.Sum256([]byte(token))
	const projectID, runID = "credential-isolation", "isolation-run"
	const runDir = "/workspace/.pwnmesh/runs/" + runID
	const mainEvidence = runDir + "/isolation-main.json"
	probe := func(destination string) string {
		return modelIsolationProbe(hex.EncodeToString(digest[:]), destination)
	}

	var mu sync.Mutex
	turns := map[string]int{}
	var modelErrors []string
	readChecks, shellChecks, childStarts := 0, 0, 0
	childrenReady := make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(message string) {
			mu.Lock()
			modelErrors = append(modelErrors, message)
			mu.Unlock()
			http.Error(w, "controlled isolation assertion failed", http.StatusBadRequest)
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("x-api-key") != token {
			fail("Dispatcher model request did not carry the configured synthetic credential")
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil || bytes.Contains(raw, []byte(token)) {
			fail("model request body unreadable or contains the synthetic credential")
			return
		}
		var input struct {
			Messages []agent.Message    `json:"messages"`
			Tools    []agent.Definition `json:"tools"`
		}
		if json.Unmarshal(raw, &input) != nil {
			fail("invalid model request JSON")
			return
		}
		session := r.Header.Get("x-opencode-session")
		child := session == runID+":isolation:left" || session == runID+":isolation:right"
		if session != runID && !child {
			fail("model request lost its root or child session identity")
			return
		}
		mu.Lock()
		turns[session]++
		turn := turns[session]
		if child && turn == 1 {
			childStarts++
			if childStarts == 2 {
				close(childrenReady)
			}
		}
		mu.Unlock()
		if child && turn == 1 {
			// A synchronous bridge would hold the first request while preventing
			// the second child from reaching the model. Require actual overlap.
			select {
			case <-childrenReady:
			case <-time.After(10 * time.Second):
				fail("concurrent child model requests did not overlap")
				return
			case <-r.Context().Done():
				fail("child model request cancelled before concurrent sibling arrived")
				return
			}
		}
		reply := scriptedModelReply{w: w, turn: turn}
		lastResult := func() (string, error) {
			return scriptedToolResult(input.Messages, fmt.Sprintf("call-%d", turn-1), false)
		}
		switch turn {
		case 1:
			reply.call("read", map[string]string{"path": "/proc/self/environ"})
		case 2:
			text, err := lastResult()
			if err != nil || strings.Contains(text, token) || strings.Contains(text, "ANTHROPIC_") || !strings.Contains(text, "PWNMESH_MODEL_BRIDGE=dispatcher-v1") {
				fail("read tool exposed provider environment or omitted bridge marker")
				return
			}
			mu.Lock()
			readChecks++
			mu.Unlock()
			destination := mainEvidence
			if child {
				destination = "isolation.json"
			}
			reply.call("bash", map[string]any{"command": probe(destination), "timeout": 30})
		case 3:
			text, err := lastResult()
			if err != nil || checkModelIsolationReceipt(text, true) != nil {
				fail("shell probe found credential/provider environment or failed to scan retained files")
				return
			}
			mu.Lock()
			shellChecks++
			mu.Unlock()
			if child {
				reply.respond(agent.Block{Type: "text", Text: "Credential isolation probe passed; original receipt is isolation.json."}, "end_turn")
				return
			}
			nodes := []map[string]any{}
			for _, id := range []string{"left", "right"} {
				nodes = append(nodes, map[string]any{"id": id, "kind": "agent", "task": "Read your process environment and run the supplied synthetic credential-isolation probe. Preserve its JSON receipt as isolation.json.", "resources": []string{}, "inputs": []any{}, "artifacts": []string{"isolation.json"}, "timeout": 45})
			}
			reply.call("run_graph", map[string]any{"key": "isolation", "parallelism": 2, "nodes": nodes})
		case 4:
			if child {
				fail("child Agent unexpectedly continued after its final account")
				return
			}
			text, err := lastResult()
			var graph struct {
				Status string `json:"status"`
				Nodes  []struct {
					ID, Status string
				} `json:"nodes"`
			}
			if err != nil || json.Unmarshal([]byte(text), &graph) != nil || graph.Status != "succeeded" || len(graph.Nodes) != 2 {
				fail("concurrent child graph did not complete successfully")
				return
			}
			seen := map[string]bool{}
			for _, node := range graph.Nodes {
				if (node.ID != "left" && node.ID != "right") || node.Status != "succeeded" || seen[node.ID] {
					fail("child graph returned an unexpected node result")
					return
				}
				seen[node.ID] = true
			}
			reply.call("finish_step", map[string]any{"fact": map[string]any{
				"description": "The main Worker and two concurrent child Agents completed synthetic credential-isolation probes without observing the Dispatcher credential.",
				"scope":       "Local synthetic credential-isolation fixture only", "observed_at": time.Now().UTC().Format(time.RFC3339Nano),
				"evidence": []map[string]string{{"path": mainEvidence}},
			}})
		default:
			fail("unexpected additional model request")
		}
	}))
	defer model.Close()

	c := config.Container{Socket: "/var/run/docker.sock", Image: image, Network: testContainerNetwork(t), Namespace: fmt.Sprintf("pwnmesh-isolation-%d", time.Now().UnixNano()), CompletedAction: "stop"}
	runner := docker.New(c)
	defer runner.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := runner.Cleanup(ctx, projectID, "deleted"); err != nil {
			t.Error(err)
		}
	}()
	runner.SetGraphHandler(func(_ context.Context, j worker.Job, request worker.GraphRequest) (any, error) {
		if request.Op != "read_updates" {
			return nil, fmt.Errorf("unexpected isolation fixture graph request %s", request.Op)
		}
		cursor := board.ExecuteUpdateCursor{ProjectID: projectID, StepID: j.Intent.ID, RunID: runID}
		if request.Updates != nil {
			cursor = *request.Updates
		}
		return board.ExecuteUpdates{Version: 1, ExecuteUpdateCursor: cursor, FromRevision: cursor.Revision, ToRevision: cursor.Revision, StateVersion: strings.Repeat("a", 64), Complete: true}, nil
	})
	intent := board.Intent{ID: "i001", From: []string{"origin"}, Description: "Verify synthetic model credential isolation in the main Worker and concurrent child Agents, retaining the probe receipt."}
	job := worker.Job{RunID: runID, Kind: "explore", WorkerType: "go", GraphRPC: true, ResultContractVersion: 2, Workspace: "/workspace", Budget: config.Task{Timeout: 120, ConcludeTimeout: 10}, Intent: &intent,
		Graph: board.Graph{Project: board.Project{ID: projectID, OrchestrationVersion: 1, Title: "Synthetic model credential isolation", Status: "active"}, Facts: []board.Fact{{ID: "origin", Description: "Local synthetic test only"}, {ID: "goal", Description: "Keep provider credentials outside all Worker tools"}}, Intents: []board.Intent{intent}}}
	backend := config.Worker{Name: "controlled", Type: "go", Env: map[string]string{
		"ANTHROPIC_BASE_URL": model.URL, "ANTHROPIC_AUTH_TOKEN": token, "ANTHROPIC_API_KEY": token, "ANTHROPIC_CUSTOM_SECRET": token,
		"ANTHROPIC_MODEL": "controlled", "PWNMESH_REQUEST_TIMEOUT": "30",
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	result, runErr := runner.Run(ctx, backend, job)
	mu.Lock()
	diagnostics := append([]string{}, modelErrors...)
	requests, reads, shells, children := turns[runID], readChecks, shellChecks, childStarts
	mu.Unlock()
	if len(diagnostics) != 0 {
		t.Fatalf("model isolation assertions failed: %v", diagnostics)
	}
	if runErr != nil || result.Status != "success" || result.Conclude || !strings.Contains(result.Text, `"outcome":"completed"`) {
		t.Fatalf("real Worker did not finish its durable evidence handoff: status=%s error=%v", result.Status, runErr)
	}
	if strings.Contains(result.Text, token) || requests != 4 || reads != 3 || shells != 3 || children != 2 {
		t.Fatalf("incomplete isolation checks: parent_requests=%d reads=%d shells=%d child_sessions=%d", requests, reads, shells, children)
	}
	// Repeat after final session, model response, child receipts and accepted
	// graph result have all been persisted. The exec receives only the digest.
	text, err := dockerExec(ctx, c.Namespace+"-dispatch-"+projectID, []string{"bash", "-c", probe("/tmp/isolation-final.json")})
	if err != nil {
		t.Fatal("final retained-file probe failed", err)
	}
	if err := checkModelIsolationReceipt(text, false); err != nil {
		t.Fatal(err)
	}
}

// Hash only candidates with the test credential's public shape. Never place
// the actual token in a tool command, model prompt or retained evidence file.
func modelIsolationProbe(digest, destination string) string {
	return `expected='` + digest + `'
credential_found=false
anthropic_env_found=false
bridge_enabled=false
[[ ${PWNMESH_MODEL_BRIDGE-} == dispatcher-v1 ]] && bridge_enabled=true
processes_checked=0
files_checked=0
jobs_checked=0
events_checked=0
model_files_checked=0
scan_file() {
  local candidate sum
  while IFS= read -r candidate; do
    sum=$(printf '%s' "$candidate" | sha256sum)
    [[ ${sum%% *} == "$expected" ]] && credential_found=true
  done < <(grep -aEo 'audit-[[:xdigit:]]{64}-token' "$1" 2>/dev/null || true)
}
for file in /proc/[0-9]*/environ; do
  [[ -r $file ]] || continue
  processes_checked=$((processes_checked + 1))
  while IFS= read -r -d '' entry; do
    [[ ${entry%%=*} == ANTHROPIC_* ]] && anthropic_env_found=true
  done < "$file" 2>/dev/null
  scan_file "$file"
done
while IFS= read -r -d '' file; do
  files_checked=$((files_checked + 1))
  case ${file##*/} in
    job.json) jobs_checked=$((jobs_checked + 1));;
    events.jsonl) events_checked=$((events_checked + 1));;
    model-*.json) model_files_checked=$((model_files_checked + 1));;
  esac
  scan_file "$file"
done < <(find /workspace -type f -print0)
printf '{"credential_found":%s,"anthropic_env_found":%s,"bridge_enabled":%s,"processes_checked":%s,"files_checked":%s,"jobs_checked":%s,"events_checked":%s,"model_files_checked":%s}\n' "$credential_found" "$anthropic_env_found" "$bridge_enabled" "$processes_checked" "$files_checked" "$jobs_checked" "$events_checked" "$model_files_checked" | tee '` + destination + `'
`
}

func checkModelIsolationReceipt(text string, requireBridge bool) error {
	var result struct {
		CredentialFound   bool `json:"credential_found"`
		AnthropicEnvFound bool `json:"anthropic_env_found"`
		BridgeEnabled     bool `json:"bridge_enabled"`
		ProcessesChecked  int  `json:"processes_checked"`
		FilesChecked      int  `json:"files_checked"`
		JobsChecked       int  `json:"jobs_checked"`
		EventsChecked     int  `json:"events_checked"`
		ModelFilesChecked int  `json:"model_files_checked"`
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return errors.New("isolation probe did not return a JSON receipt")
	}
	if result.CredentialFound || result.AnthropicEnvFound || requireBridge && !result.BridgeEnabled {
		return errors.New("provider credential or environment reached the Worker")
	}
	if result.ProcessesChecked < 1 || result.FilesChecked < 1 || result.JobsChecked < 1 || result.EventsChecked < 1 || result.ModelFilesChecked < 1 {
		return fmt.Errorf("isolation probe omitted required process or retained-file categories: %+v", result)
	}
	return nil
}
