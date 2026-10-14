package docker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/modelrpc"
	"pwnmesh/internal/protocol"
)

func TestGraphBridgeDeliversOversizedSuccessfulActionAsAcknowledgement(t *testing.T) {
	var published []byte
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PUT" && strings.Contains(r.URL.Path, "/archive"):
			archive := tar.NewReader(r.Body)
			for {
				header, err := archive.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				if header.Typeflag == tar.TypeReg {
					published, err = io.ReadAll(archive)
					if err != nil {
						t.Error(err)
					}
				}
			}
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/exec"):
			io.WriteString(w, `{"Id":"publish"}`)
		case r.URL.Path == "/exec/publish/start":
		case r.URL.Path == "/exec/publish/json":
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected engine route: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer engine.Close()
	client := &Client{http: &http.Client{Transport: graphBridgeTestTransport{base: http.DefaultTransport, endpoint: engine.URL}}}
	version := strings.Repeat("b", 64)
	handlerCalls := 0
	client.SetGraphHandler(func(context.Context, protocol.Job, protocol.GraphRequest) (any, error) {
		handlerCalls++
		return board.StateActionResult{Op: "candidate", ID: "candidate_many", Revision: 42, StateVersion: version, Result: json.RawMessage(`{"evidence":"` + strings.Repeat("x", protocol.MaxGraphRPCBytes) + `"}`)}, nil
	})
	request := protocol.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "graph_action", Action: board.StateAction{Op: "candidate", IdempotencyKey: "observed", Payload: json.RawMessage(`{}`)}}
	if err := client.graphBridge(context.Background(), "fixture", "/workspace/.pwnmesh/runs/run", protocol.Job{Kind: "explore", GraphRPC: true})(request); err != nil {
		t.Fatal(err)
	}
	var response protocol.GraphResponse
	if err := json.Unmarshal(published, &response); err != nil {
		t.Fatal(err)
	}
	if handlerCalls != 1 || response.Error != "" {
		t.Fatalf("successful action did not reach its handler: calls=%d response=%s", handlerCalls, published)
	}
	var receipt struct {
		board.StateActionResult
		ResultOmitted bool `json:"result_omitted"`
	}
	if err := json.Unmarshal(response.Result, &receipt); err != nil {
		t.Fatal(err)
	}
	if len(published) > protocol.MaxGraphRPCBytes || response.RequestID != request.RequestID || !receipt.ResultOmitted || receipt.Op != "candidate" || receipt.ID != "candidate_many" || receipt.StateVersion != version || receipt.Revision != 42 {
		t.Fatalf("successful mutation lost its acknowledgement: %s", published)
	}
}

func TestBridgeResponsesPublishOnlyAfterCompleteArchive(t *testing.T) {
	for _, bridge := range []string{"graph", "model"} {
		for _, failure := range []string{"", "archive", "rename"} {
			t.Run(bridge+"/"+failure, func(t *testing.T) {
				requestID := strings.Repeat("a", 32)
				target := "/workspace/.pwnmesh/runs/run/" + bridge + "-response-" + requestID + ".json"
				archived, renamed := false, false
				client := &Client{http: &http.Client{Transport: inputTestTransport(func(r *http.Request) (*http.Response, error) {
					status, body := http.StatusOK, ""
					switch r.Method + " " + r.URL.Path {
					case "PUT /containers/fixture/archive":
						archive := tar.NewReader(r.Body)
						for {
							header, err := archive.Next()
							if err == io.EOF {
								break
							}
							if err != nil {
								t.Fatal(err)
							}
							if header.Typeflag != tar.TypeReg {
								continue
							}
							if header.Name != strings.TrimPrefix(target, "/workspace/")+".tmp" {
								t.Fatalf("response exposed before publication: %s", header.Name)
							}
							var response struct {
								RequestID string `json:"request_id"`
							}
							if err := json.NewDecoder(archive).Decode(&response); err != nil || response.RequestID != requestID {
								t.Fatalf("incomplete response archive: %+v, %v", response, err)
							}
							archived = true
						}
						if failure == "archive" {
							status = http.StatusInternalServerError
						}
					case "POST /containers/fixture/exec":
						var command struct {
							Cmd []string
						}
						if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
							t.Fatal(err)
						}
						if !archived || failure == "archive" || !reflect.DeepEqual(command.Cmd, []string{"/bin/mv", "-T", "--", target + ".tmp", target}) {
							t.Fatalf("invalid publication: archived=%v command=%v", archived, command.Cmd)
						}
						renamed = true
						body = `{"Id":"publish"}`
					case "POST /exec/publish/start":
					case "GET /exec/publish/json":
						body = `{"ExitCode":0}`
						if failure == "rename" {
							body = `{"ExitCode":1}`
						}
					default:
						t.Fatalf("unexpected Engine request: %s %s", r.Method, r.URL.Path)
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}}
				var err error
				if bridge == "graph" {
					client.SetGraphHandler(func(context.Context, protocol.Job, protocol.GraphRequest) (any, error) { return map[string]any{}, nil })
					err = client.graphBridge(context.Background(), "fixture", "/workspace/.pwnmesh/runs/run", protocol.Job{Kind: "explore", GraphRPC: true})(protocol.GraphRequest{RequestID: requestID, Op: "read_graph"})
				} else {
					err = client.modelDelivery("fixture", "/workspace/.pwnmesh/runs/run")(context.Background(), modelrpc.Response{RequestID: requestID})
				}
				if (err != nil) != (failure != "") || !archived || renamed != (failure != "archive") {
					t.Fatalf("publication result: err=%v archived=%v renamed=%v", err, archived, renamed)
				}
			})
		}
	}
}

type graphBridgeTestTransport struct {
	base     http.RoundTripper
	endpoint string
}

func (transport graphBridgeTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	forward, err := http.NewRequestWithContext(request.Context(), request.Method, transport.endpoint+request.URL.RequestURI(), request.Body)
	if err != nil {
		return nil, err
	}
	forward.Header = request.Header
	return transport.base.RoundTrip(forward)
}
