package docker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
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
	client.SetGraphHandler(func(context.Context, worker.Job, worker.GraphRequest) (any, error) {
		handlerCalls++
		return board.StateActionResult{Op: "candidate", ID: "candidate_many", Revision: 42, StateVersion: version, Result: json.RawMessage(`{"evidence":"` + strings.Repeat("x", worker.MaxGraphRPCBytes) + `"}`)}, nil
	})
	request := worker.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "graph_action", Action: board.StateAction{Op: "candidate", IdempotencyKey: "observed", Payload: json.RawMessage(`{}`)}}
	if err := client.graphBridge(context.Background(), "fixture", "/workspace/.pwnmesh/runs/run", worker.Job{Kind: "explore", GraphRPC: true})(request); err != nil {
		t.Fatal(err)
	}
	var response worker.GraphResponse
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
	if len(published) > worker.MaxGraphRPCBytes || response.RequestID != request.RequestID || !receipt.ResultOmitted || receipt.Op != "candidate" || receipt.ID != "candidate_many" || receipt.StateVersion != version || receipt.Revision != 42 {
		t.Fatalf("successful mutation lost its acknowledgement: %s", published)
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
