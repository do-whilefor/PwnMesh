package docker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/protocol"
)

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
