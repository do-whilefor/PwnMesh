package docker

import (
	"archive/tar"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/worker"
)

func TestRunCrashConfirmsInterruptionBeforeSameRunRecovery(t *testing.T) {
	for _, failure := range []string{"exit_137", "missing_result", "invalid_final_result"} {
		t.Run(failure, func(t *testing.T) {
			var order []string
			var jobs, tokens []string
			execs := map[string]bool{}
			attempt := 0
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/containers/test-dispatch-p/json":
					io.WriteString(w, `{"Image":"sha256:worker","Config":{"Labels":{"pwnmesh.namespace":"test","pwnmesh.project":"p"}},"State":{"Running":true},"HostConfig":{"NetworkMode":"bridge"}}`)
				case r.URL.Path == "/images/worker/json":
					io.WriteString(w, `{"Id":"sha256:worker"}`)
				case strings.HasSuffix(r.URL.Path, "/archive"):
					reader := tar.NewReader(r.Body)
					for {
						header, err := reader.Next()
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						if header.Typeflag == tar.TypeReg {
							data, _ := io.ReadAll(reader)
							if strings.HasSuffix(header.Name, "/job.json") {
								jobs = append(jobs, string(data))
							} else if strings.HasSuffix(header.Name, "/launch-token") {
								tokens = append(tokens, string(data))
							}
						}
					}
				case strings.HasSuffix(r.URL.Path, "/exec"):
					var input struct{ Cmd []string }
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Cmd) < 3 {
						t.Errorf("invalid exec input: %+v %v", input, err)
						w.WriteHeader(500)
						return
					}
					interrupted := input.Cmd[2] == "--interrupt"
					if interrupted {
						order = append(order, "interrupt")
					} else {
						attempt++
						order = append(order, "worker")
					}
					id := fmt.Sprintf("%d", len(execs)+1)
					execs[id] = interrupted
					fmt.Fprintf(w, `{"Id":%q}`, id)
				case strings.HasSuffix(r.URL.Path, "/start"):
					id := strings.Split(r.URL.Path, "/")[2]
					if !execs[id] {
						output := `{"type":"result","status":"success","text":"settled"}` + "\n"
						if attempt == 1 && failure == "missing_result" {
							output = `{"type":"message_end"}` + "\n"
						} else if attempt == 1 && failure == "invalid_final_result" {
							output = `{"type":"result","status":99}` // Exercise final unterminated frame.
						}
						var header [8]byte
						header[0] = 1
						binary.BigEndian.PutUint32(header[4:], uint32(len(output)))
						w.Write(header[:])
						io.WriteString(w, output)
					}
				case strings.HasSuffix(r.URL.Path, "/json") && strings.HasPrefix(r.URL.Path, "/exec/"):
					id := strings.Split(r.URL.Path, "/")[2]
					exit := 0
					if !execs[id] && attempt == 1 && failure == "exit_137" {
						exit = 137
					}
					fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, exit)
				default:
					t.Errorf("unexpected route: %s %s", r.Method, r.URL)
					w.WriteHeader(500)
				}
			}))
			defer engine.Close()
			client := &Client{Config: config.Container{Namespace: "test", Image: "worker"}, http: &http.Client{Transport: graphBridgeTestTransport{base: http.DefaultTransport, endpoint: engine.URL}}}
			job := worker.Job{RunID: "same-run", Kind: "explore", Graph: board.Graph{Project: board.Project{ID: "p"}}}
			result, err := client.Run(context.Background(), config.Worker{}, job)
			if err == nil || result.Status != "" || !reflect.DeepEqual(order, []string{"worker", "interrupt"}) {
				t.Fatalf("failed worker was accepted or did not settle: result=%+v err=%v order=%v", result, err, order)
			}
			result, err = client.Run(context.Background(), config.Worker{}, job)
			if err != nil || result.Status != "success" || !reflect.DeepEqual(order, []string{"worker", "interrupt", "worker"}) {
				t.Fatalf("same-run recovery failed: %+v %v %v", result, err, order)
			}
			if len(jobs) != 2 || jobs[0] != jobs[1] || len(tokens) != 2 || tokens[0] == tokens[1] {
				t.Fatal("recovery changed the job or reused the invalidated launch token")
			}
		})
	}
}
