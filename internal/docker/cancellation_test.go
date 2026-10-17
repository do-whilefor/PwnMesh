package docker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/protocol"
)

func TestRunPreservesOnlyDecodedMetricsAfterFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cancel     bool
		stopFailed bool
	}{
		{name: "worker exit"},
		{name: "cancellation", cancel: true},
		{name: "unconfirmed stop", stopFailed: true},
		{name: "unconfirmed cancellation", cancel: true, stopFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			metrics := &protocol.DecisionMetrics{
				Version: 1, UsageScope: "run_and_children", ModelCalls: 2, UsageCalls: 2,
				Usage:       agent.Usage{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 5, CacheWriteTokens: 3},
				UsageStatus: "reported_only", CostStatus: "unknown",
				ChildUsage: &protocol.ChildUsage{Sessions: 1, ModelCalls: 1, UsageCalls: 1,
					Usage: agent.Usage{InputTokens: 2, OutputTokens: 1, CacheReadTokens: 1, CacheWriteTokens: 1}},
			}
			output, err := json.Marshal(protocol.Result{
				Type: "result", Status: "success", Text: "must not be accepted", Conclude: true,
				StateVersion: "old-state", Metrics: metrics,
			})
			if err != nil {
				t.Fatal(err)
			}
			output = append(output, '\n')
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/containers/test-dispatch-p/json":
					io.WriteString(w, `{"Id":"verified-id","Image":"sha256:worker","Config":{"Labels":{"pwnmesh.namespace":"test","pwnmesh.project":"p","pwnmesh.model-boundary":"dispatcher-v1"}},"State":{"Running":true},"HostConfig":{"NetworkMode":"bridge"}}`)
				case r.URL.Path == "/images/worker/json":
					io.WriteString(w, `{"Id":"sha256:worker"}`)
				case strings.HasSuffix(r.URL.Path, "/archive"):
					_, _ = io.Copy(io.Discard, r.Body)
				case strings.HasSuffix(r.URL.Path, "/exec"):
					var input struct{ Cmd []string }
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Cmd) < 3 {
						t.Error("invalid command")
						w.WriteHeader(500)
						return
					}
					if input.Cmd[2] == "--job" {
						io.WriteString(w, `{"Id":"worker"}`)
					} else if tc.stopFailed {
						w.WriteHeader(503)
					} else {
						io.WriteString(w, `{"Id":"helper"}`)
					}
				case r.URL.Path == "/exec/worker/start":
					var header [8]byte
					header[0] = 1
					binary.BigEndian.PutUint32(header[4:], uint32(len(output)))
					w.Write(header[:])
					w.Write(output)
				case r.URL.Path == "/exec/worker/json":
					// Inspection starts only after the complete result was consumed.
					if tc.cancel {
						cancel()
					}
					io.WriteString(w, `{"Running":false,"ExitCode":1}`)
				case r.URL.Path == "/exec/helper/start":
				case r.URL.Path == "/exec/helper/json":
					io.WriteString(w, `{"Running":false,"ExitCode":0}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(500)
				}
			}))
			defer engine.Close()
			c := boundaryClient(engine.URL)
			job := protocol.Job{RunID: "run-1", Kind: "explore", Graph: board.Graph{Project: board.Project{ID: "p"}}}
			backend := config.Worker{Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "fixture-model-auth", "ANTHROPIC_MODEL": "fixture-model"}}
			result, err := c.Run(ctx, backend, job)
			if err == nil || (tc.cancel && !errors.Is(err, context.Canceled)) {
				t.Fatalf("execution failure lost: %v", err)
			}
			if tc.stopFailed != strings.Contains(err.Error(), "unconfirmed") {
				t.Fatalf("stop outcome changed: %v", err)
			}
			if !reflect.DeepEqual(result, protocol.Result{Metrics: metrics}) {
				t.Fatalf("decoded usage was lost or failed execution content escaped: %+v", result)
			}
			_, quarantined := c.pendingStops.Load("/workspace/.pwnmesh/runs/run-1")
			if quarantined != tc.stopFailed {
				t.Fatalf("stop quarantine changed: %v", quarantined)
			}
		})
	}
}

func TestUnconfirmedStopBlocksNextRunUntilDockerConfirms(t *testing.T) {
	for _, hard := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancel", false: "interrupt"}[hard], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var controlAvailable atomic.Bool
			var launches, recoveries atomic.Int32
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/containers/test-dispatch-p/json":
					io.WriteString(w, `{"Id":"verified-id","Image":"sha256:worker","Config":{"Labels":{"pwnmesh.namespace":"test","pwnmesh.project":"p","pwnmesh.model-boundary":"dispatcher-v1"}},"State":{"Running":true},"HostConfig":{"NetworkMode":"bridge"}}`)
				case r.URL.Path == "/images/worker/json":
					io.WriteString(w, `{"Id":"sha256:worker"}`)
				case strings.HasSuffix(r.URL.Path, "/archive"):
					_, _ = io.Copy(io.Discard, r.Body)
				case strings.HasSuffix(r.URL.Path, "/exec"):
					var input struct{ Cmd []string }
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Cmd) < 3 {
						t.Error("invalid command")
						w.WriteHeader(500)
						return
					}
					if input.Cmd[2] == "--job" {
						if launches.Add(1) == 1 {
							if hard {
								cancel()
							}
							w.WriteHeader(500)
							return
						}
						io.WriteString(w, `{"Id":"worker"}`)
						return
					}
					if !controlAvailable.Load() {
						w.WriteHeader(503)
						return
					}
					if r.URL.Path != "/containers/verified-id/exec" {
						t.Error("recovery did not use verified immutable container ID")
					}
					if hard && (input.Cmd[2] != "--cancel" || input.Cmd[len(input.Cmd)-1] != "--force") {
						t.Errorf("cancel recovery: %v", input.Cmd)
					}
					if !hard && input.Cmd[2] != "--interrupt" {
						t.Errorf("recovery changed a resumable interruption into cancellation: %v", input.Cmd)
					}
					recoveries.Add(1)
					io.WriteString(w, `{"Id":"helper"}`)
				case r.URL.Path == "/exec/worker/start":
					output := "{\"type\":\"result\",\"status\":\"success\",\"text\":\"done\"}\n"
					var header [8]byte
					header[0] = 1
					binary.BigEndian.PutUint32(header[4:], uint32(len(output)))
					w.Write(header[:])
					io.WriteString(w, output)
				case r.URL.Path == "/exec/helper/start":
				case strings.HasPrefix(r.URL.Path, "/exec/") && strings.HasSuffix(r.URL.Path, "/json"):
					io.WriteString(w, `{"Running":false,"ExitCode":0}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(500)
				}
			}))
			defer engine.Close()
			c := boundaryClient(engine.URL)
			j := protocol.Job{RunID: "run-1", Kind: "explore", Graph: board.Graph{Project: board.Project{ID: "p"}}}
			backend := config.Worker{Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "fixture-model-auth", "ANTHROPIC_MODEL": "fixture-model"}}
			if _, err := c.Run(ctx, backend, j); err == nil || !strings.Contains(err.Error(), "unconfirmed") {
				t.Fatalf("unconfirmed stop was accepted: %v", err)
			}
			if err := c.confirmPendingStops(context.Background(), "other-project"); err != nil {
				t.Fatal(err)
			}
			j.RunID = "run-2"
			if _, err := c.Run(context.Background(), backend, j); err == nil || !strings.Contains(err.Error(), "unconfirmed") {
				t.Fatalf("new run escaped quarantine: %v", err)
			}
			if launches.Load() != 1 {
				t.Fatal("started a new worker while old execution was unconfirmed")
			}
			controlAvailable.Store(true)
			result, err := c.Run(context.Background(), backend, j)
			if err != nil || result.Status != "success" || recoveries.Load() != 1 || launches.Load() != 2 {
				t.Fatalf("recovery failed: %+v %v recoveries=%d launches=%d", result, err, recoveries.Load(), launches.Load())
			}
			c.pendingStops.Range(func(_, _ any) bool { t.Error("confirmed stop remained quarantined"); return true })
		})
	}
}

func TestAbsentOrStoppedContainerClearsPendingStop(t *testing.T) {
	for _, absent := range []bool{false, true} {
		engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Error("stopped container executed a helper")
			}
			if absent {
				w.WriteHeader(404)
				return
			}
			io.WriteString(w, `{"Id":"stopped","Config":{"Labels":{"pwnmesh.namespace":"test","pwnmesh.project":"p"}},"State":{"Running":false}}`)
		}))
		c := boundaryClient(engine.URL)
		c.pendingStops.Store("/workspace/.pwnmesh/runs/run-1", pendingStop{project: "p", hard: true})
		if err := c.confirmPendingStops(context.Background(), "p"); err != nil {
			t.Fatal(err)
		}
		c.pendingStops.Range(func(_, _ any) bool { t.Error("stopped container remained quarantined"); return true })
		engine.Close()
	}
}
