//go:build linux

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
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

// No upstream forwarding is implemented: both reserved .invalid destinations
// exist only in this local proxy. Without proxy routing, the test cannot pass.
func TestDockerModelAndWorkerUseSameConfiguredProxy(t *testing.T) {
	image := os.Getenv("PWNMESH_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set PWNMESH_DOCKER_TEST_IMAGE for real model and worker proxy acceptance")
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var bindIP net.IP
	for _, address := range addresses {
		ip, _, parseErr := net.ParseCIDR(address.String())
		if parseErr == nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			bindIP = ip.To4()
			break
		}
	}
	if bindIP == nil {
		t.Fatal("proxy acceptance requires a non-loopback IPv4 interface")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(bindIP.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	const projectID = "proxy-project"
	const runID = "proxy-run"
	const runDir = "/workspace/.pwnmesh/runs/" + runID
	const credential = "synthetic-proxy-integration-credential"
	const marker = "PWNMESH_SYNTHETIC_PROXY_OK"
	var mu sync.Mutex
	modelCalls, workerCalls := 0, 0
	var failures []string
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !r.URL.IsAbs() || r.URL.Scheme != "http" {
			failures = append(failures, "request was not addressed through an HTTP forward proxy")
			http.Error(w, "absolute proxy URL required", 400)
			return
		}
		switch r.URL.Host {
		case "worker.example.invalid":
			workerCalls++
			if r.Method != "GET" || r.URL.Path != "/check" || r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
				failures = append(failures, "worker request had the wrong target or included model authentication")
				http.Error(w, "invalid synthetic worker request", 400)
				return
			}
			_, _ = fmt.Fprintln(w, marker)
		case "model.example.invalid":
			modelCalls++
			if r.Method != "POST" || r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != credential || r.Header.Get("anthropic-version") != "2023-06-01" {
				failures = append(failures, "model request did not use the configured Messages protocol")
				http.Error(w, "invalid synthetic model request", 400)
				return
			}
			var payload struct {
				Messages []agent.Message    `json:"messages"`
				Tools    []agent.Definition `json:"tools"`
				Model    string             `json:"model"`
				Stream   bool               `json:"stream"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Model != "proxy-fixture" || !payload.Stream {
				failures = append(failures, "invalid model payload")
				http.Error(w, "invalid synthetic payload", 400)
				return
			}
			respond := func(block agent.Block, stop string) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"role": "assistant", "content": []agent.Block{block}, "stop_reason": stop})
			}
			switch modelCalls {
			case 1:
				hasBash := false
				for _, tool := range payload.Tools {
					hasBash = hasBash || tool.Name == "bash"
				}
				if !hasBash {
					failures = append(failures, "worker did not advertise bash")
				}
				// The credential never appears in a tool command or worker input.
				command := "set -eu; env > " + runDir + "/worker-env.txt; curl --fail --silent --show-error --max-time 10 http://worker.example.invalid/check > " + runDir + "/proxy-marker.txt; cat " + runDir + "/proxy-marker.txt"
				input, _ := json.Marshal(map[string]any{"command": command, "timeout": 15})
				respond(agent.Block{Type: "tool_use", ID: "proxy-curl", Name: "bash", Input: input}, "tool_use")
			case 2:
				validReceipt := false
				if len(payload.Messages) > 0 {
					for _, block := range payload.Messages[len(payload.Messages)-1].Content {
						if block.Type == "tool_result" && block.ToolUseID == "proxy-curl" && !block.IsError && strings.Contains(string(block.Content), marker) {
							validReceipt = true
						}
					}
				}
				if !validReceipt || workerCalls != 1 {
					failures = append(failures, "model did not receive the successful proxied curl result")
				}
				result, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": map[string]any{
					"description": "A synthetic worker HTTP request reached the configured project proxy.", "scope": "local proxy fixture", "observed_at": time.Now().UTC().Format(time.RFC3339), "evidence": []map[string]string{{"path": runDir + "/proxy-marker.txt"}},
				}}})
				respond(agent.Block{Type: "text", Text: string(result)}, "end_turn")
			default:
				failures = append(failures, "unexpected additional model request")
				http.Error(w, "unexpected synthetic model request", 400)
			}
		default:
			failures = append(failures, "unexpected destination; no forwarding is permitted")
			http.Error(w, "unknown synthetic destination", 400)
		}
	}))
	_ = proxy.Listener.Close()
	proxy.Listener = listener
	proxy.Start()
	defer proxy.Close()
	c := config.Container{Socket: "/var/run/docker.sock", Image: image, Network: testContainerNetwork(t), Namespace: fmt.Sprintf("pwnmesh-proxy-%d", time.Now().UnixNano()), CompletedAction: "stop"}
	runner := docker.New(c)
	defer runner.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := runner.Cleanup(ctx, projectID, "deleted"); err != nil {
			t.Error(err)
		}
	}()
	runner.SetGraphHandler(func(_ context.Context, j worker.Job, r worker.GraphRequest) (any, error) {
		if r.Op != "read_updates" {
			return nil, fmt.Errorf("unexpected proxy fixture graph request %s", r.Op)
		}
		cursor := board.ExecuteUpdateCursor{ProjectID: projectID, StepID: j.Intent.ID, RunID: runID}
		if r.Updates != nil {
			cursor = *r.Updates
		}
		return board.ExecuteUpdates{Version: 1, ExecuteUpdateCursor: cursor, FromRevision: cursor.Revision, ToRevision: cursor.Revision, StateVersion: strings.Repeat("a", 64), Complete: true}, nil
	})
	intent := board.Intent{ID: "proxy-step", From: []string{"origin"}, Description: "Request the local synthetic HTTP fixture through the configured proxy and retain its marker."}
	job := worker.Job{RunID: runID, Kind: "explore", WorkerType: "go", GraphRPC: true, ResultContractVersion: 2, Workspace: "/workspace", Budget: config.Task{Timeout: 45, ConcludeTimeout: 10}, Intent: &intent, Graph: board.Graph{
		Project: board.Project{ID: projectID, OrchestrationVersion: 1, Title: "Synthetic proxy acceptance", Status: "active"},
		Facts:   []board.Fact{{ID: "origin", Description: "Local synthetic proxy only; do not contact any real service."}, {ID: "goal", Description: "Verify model and worker routing through one proxy."}}, Intents: []board.Intent{intent},
	}}
	backend := config.Worker{Name: "proxy-controlled", Type: "go", Env: map[string]string{"ANTHROPIC_BASE_URL": "http://model.example.invalid", "ANTHROPIC_AUTH_TOKEN": credential, "ANTHROPIC_MODEL": "proxy-fixture", "PWNMESH_CONNECTION_MODE": "proxy", "PWNMESH_PROXY_URL": proxy.URL, "PWNMESH_REQUEST_TIMEOUT": "15", "PWNMESH_MAX_OUTPUT_TOKENS": "2048"}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, backend, job)
	if err != nil || result.Status != "success" || result.Conclude {
		t.Fatalf("proxied worker failed: status=%s conclude=%v error=%v", result.Status, result.Conclude, err)
	}
	container := c.Namespace + "-dispatch-" + projectID
	environment, err := dockerExec(ctx, container, []string{"cat", runDir + "/worker-env.txt"})
	if err != nil {
		t.Fatal("cannot inspect synthetic worker environment", err)
	}
	if strings.Contains(environment, credential) || strings.Contains(environment, "ANTHROPIC_") {
		t.Fatal("model credentials or provider configuration entered the worker environment")
	}
	values := map[string]string{}
	for _, line := range strings.Split(environment, "\n") {
		key, value, _ := strings.Cut(line, "=")
		values[key] = value
	}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if values[key] != proxy.URL {
			t.Errorf("worker %s does not use the configured proxy", key)
		}
	}
	proof, err := dockerExec(ctx, container, []string{"cat", runDir + "/proxy-marker.txt"})
	if err != nil || strings.TrimSpace(proof) != marker {
		t.Fatal("worker did not preserve the synthetic proxy response")
	}
	mu.Lock()
	defer mu.Unlock()
	if modelCalls != 2 || workerCalls != 1 || len(failures) > 0 {
		t.Fatalf("proxy verification: model=%d worker=%d failures=%v", modelCalls, workerCalls, failures)
	}
}
