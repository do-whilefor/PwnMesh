//go:build linux

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/server"
)

// This opt-in accepts an external browser request through the unchanged
// production handler. The harness never creates, repairs or rewrites it.
// Browser provenance is recorded separately by the operator; HTTP alone
// cannot distinguish a browser from another external client.
type liveWebTask struct {
	Title                string `json:"title"`
	Origin               string `json:"origin"`
	Goal                 string `json:"goal"`
	Scenario             string `json:"scenario"`
	OrchestrationVersion int    `json:"orchestration_version"`
}

type liveWebCreation struct {
	Source         string      `json:"source"`
	ReadyAt        time.Time   `json:"ready_at"`
	RequestStarted time.Time   `json:"request_started,omitempty"`
	ResponseAt     time.Time   `json:"response_at,omitempty"`
	ProjectID      string      `json:"project_id,omitempty"`
	Status         int         `json:"http_status,omitempty"`
	Failure        string      `json:"failure,omitempty"`
	Graph          board.Graph `json:"-"`
}

type liveWebCreationGate struct {
	want   liveWebTask
	mu     sync.Mutex
	result liveWebCreation
	notice chan struct{}
	writes int
	active bool
}

func newLiveWebCreationGate(want liveWebTask) *liveWebCreationGate {
	return &liveWebCreationGate{want: want, notice: make(chan struct{}, 1), result: liveWebCreation{Source: "external_http_project_creation", ReadyAt: time.Now().UTC()}}
}

func liveWebWaitDuration(raw string) (time.Duration, error) {
	seconds := 900
	if raw != "" {
		var err error
		seconds, err = strconv.Atoi(raw)
		if err != nil || seconds < 1 || seconds > 3600 {
			return 0, errors.New("PWNMESH_LIVE_WEB_WAIT_SECONDS must be between 1 and 3600")
		}
	}
	return time.Duration(seconds) * time.Second, nil
}

func newLiveObservedAPI(handler http.Handler, address string) (*httptest.Server, error) {
	if address == "" {
		return httptest.NewServer(handler), nil
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	api := httptest.NewUnstartedServer(handler)
	_ = api.Listener.Close()
	api.Listener = listener
	api.Start()
	// The dispatcher shares this process's namespace, including when Docker
	// publishes the explicitly requested port for the operator's browser.
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		api.Close()
		return nil, err
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	api.URL = "http://" + net.JoinHostPort(host, port)
	return api, nil
}

type liveCreationResponse struct {
	*auditResponse
	body bytes.Buffer
}

func (w *liveCreationResponse) Write(raw []byte) (int, error) {
	n, err := w.auditResponse.Write(raw)
	_, _ = w.body.Write(raw[:n])
	return n, err
}

func (g *liveWebCreationGate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		watch := !g.active && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		if watch {
			g.writes++
			if g.writes > 1 && g.result.Failure == "" {
				g.result.Failure = "multiple mutations occurred before dispatcher start; a modified or replacement project is not the original Web creation"
			}
		}
		g.mu.Unlock()
		if !watch {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now().UTC()
		out := &liveCreationResponse{auditResponse: &auditResponse{ResponseWriter: w}}
		next.ServeHTTP(out, r)
		finished := time.Now().UTC()
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.result.RequestStarted.IsZero() {
			g.result.RequestStarted, g.result.ResponseAt, g.result.Status = started, finished, out.status
		}
		var graph board.Graph
		var err error
		if r.Method != http.MethodPost || r.URL.Path != "/projects" {
			err = errors.New("first Web mutation must create the project")
		} else if out.status != http.StatusCreated {
			err = fmt.Errorf("Web project creation returned HTTP %d", out.status)
		} else if err = json.Unmarshal(out.body.Bytes(), &graph); err == nil {
			err = matchLiveWebProject(graph, g.want)
		}
		if err != nil && g.result.Failure == "" {
			g.result.Failure = err.Error()
		}
		if g.result.ProjectID == "" {
			g.result.ProjectID, g.result.Graph = graph.Project.ID, graph
		}
		select {
		case g.notice <- struct{}{}:
		default:
		}
	})
}

func matchLiveWebProject(graph board.Graph, want liveWebTask) error {
	p := graph.Project
	if p.ID == "" || p.Title != want.Title || p.Scenario != want.Scenario || p.OrchestrationVersion != want.OrchestrationVersion {
		return errors.New("Web-created project title, scenario or orchestration version does not match web-task.json")
	}
	if p.Status != "active" || p.Bootstrap || p.Generation != 0 || p.Reason != nil || p.Curator != nil || p.RestartedAt != "" || p.TerminatedAt != "" || len(graph.Intents) != 0 || len(graph.Hints) != 0 || len(graph.Facts) != 2 {
		return errors.New("Web-created project is not a fresh, unmodified project")
	}
	facts := map[string]string{}
	for _, fact := range graph.Facts {
		facts[fact.ID] = fact.Description
	}
	if facts["origin"] != want.Origin || facts["goal"] != want.Goal {
		return errors.New("Web-created project origin or goal does not match web-task.json")
	}
	return nil
}

func (g *liveWebCreationGate) snapshot() liveWebCreation {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.result
}

func (g *liveWebCreationGate) wait(ctx context.Context) (liveWebCreation, error) {
	select {
	case <-ctx.Done():
		result := g.snapshot()
		result.Failure = "no acceptable external Web creation before wait deadline: " + ctx.Err().Error()
		return result, errors.New(result.Failure)
	case <-g.notice:
		result := g.snapshot()
		if result.Failure != "" {
			return result, errors.New(result.Failure)
		}
		return result, nil
	}
}

func (g *liveWebCreationGate) activate() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.result.Failure != "" {
		return errors.New(g.result.Failure)
	}
	if g.writes != 1 || g.result.ProjectID == "" {
		return errors.New("dispatcher requires exactly one accepted external Web creation")
	}
	g.active = true
	return nil
}

func TestLiveWebCreationRequiresOriginalMatchingProject(t *testing.T) {
	want := liveWebTask{Title: "Browser project", Origin: "exact origin", Goal: "exact goal", Scenario: "pentest", OrchestrationVersion: 1}
	fixture := func() board.Graph {
		return board.Graph{Project: board.Project{ID: "proj_001", Title: want.Title, Status: "active", Scenario: want.Scenario, OrchestrationVersion: 1}, Facts: []board.Fact{{ID: "origin", Description: want.Origin}, {ID: "goal", Description: want.Goal}}}
	}
	for _, test := range []struct {
		name   string
		change func(*board.Graph)
	}{
		{"valid", func(*board.Graph) {}},
		{"legacy", func(g *board.Graph) { g.Project.OrchestrationVersion = 0 }},
		{"goal", func(g *board.Graph) { g.Facts[1].Description = "other goal" }},
		{"origin", func(g *board.Graph) { g.Facts[0].Description = "other origin" }},
		{"scenario", func(g *board.Graph) { g.Project.Scenario = "ctf" }},
		{"restarted", func(g *board.Graph) { g.Project.Generation = 1 }},
		{"hints", func(g *board.Graph) { g.Hints = []board.Hint{{Content: "changed"}} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			graph := fixture()
			test.change(&graph)
			gate := newLiveWebCreationGate(want)
			handler := gate.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(graph)
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/projects", nil))
			result, err := gate.wait(context.Background())
			if (err == nil) != (test.name == "valid") || result.RequestStarted.IsZero() || result.ResponseAt.Before(result.RequestStarted) || response.Code != http.StatusCreated {
				t.Fatalf("invalid external creation result: %+v, %v", result, err)
			}
			if test.name == "valid" {
				if err := gate.activate(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	t.Run("modified_then_matching", func(t *testing.T) {
		gate := newLiveWebCreationGate(want)
		handler := gate.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(fixture())
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/projects/proj_001/title", nil))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/projects", nil))
		if _, err := gate.wait(context.Background()); err == nil || gate.activate() == nil {
			t.Fatal("accepted a project after an earlier mutation")
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		gate := newLiveWebCreationGate(want)
		handler := gate.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(fixture())
		}))
		for range 2 {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/projects", nil))
		}
		if _, err := gate.wait(context.Background()); err == nil || gate.activate() == nil {
			t.Fatal("accepted multiple Web creations")
		}
	})
	t.Run("no_creation", func(t *testing.T) {
		gate := newLiveWebCreationGate(want)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := gate.wait(ctx); err == nil || gate.activate() == nil {
			t.Fatal("missing Web creation counted as success")
		}
	})
}

func TestLiveWebWaitBoundsAndListener(t *testing.T) {
	for _, raw := range []string{"0", "-1", "3601", "forever"} {
		if _, err := liveWebWaitDuration(raw); err == nil {
			t.Fatalf("accepted wait %q", raw)
		}
	}
	if duration, err := liveWebWaitDuration(""); err != nil || duration != 15*time.Minute {
		t.Fatalf("unexpected default wait: %s, %v", duration, err)
	}
	api, err := newLiveObservedAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	response, err := http.Get(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatal("explicit Web listener did not serve production handler")
	}
}

func TestLiveWebCreationObservesUnchangedProductionHandler(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "project.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	want := liveWebTask{Title: "Browser project", Origin: "exact origin", Goal: "exact goal", Scenario: "pentest", OrchestrationVersion: 1}
	gate := newLiveWebCreationGate(want)
	api, err := newLiveObservedAPI(gate.wrap(server.New(store)), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	for _, path := range []string{"/", "/static/data.js", "/projects"} {
		response, err := http.Get(api.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("production Web request %s returned %d", path, response.StatusCode)
		}
	}
	if gate.activate() == nil {
		t.Fatal("read-only browser navigation authorized dispatch")
	}
	body, _ := json.Marshal(want)
	response, err := http.Post(api.URL+"/projects", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var original board.Graph
	err = json.NewDecoder(response.Body).Decode(&original)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("unchanged production creation failed: %d, %v", response.StatusCode, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	observed, err := gate.wait(ctx)
	if err != nil || observed.ProjectID != original.Project.ID || observed.Source != "external_http_project_creation" {
		t.Fatalf("observer changed production response: %+v, %v", observed, err)
	}
	if err = gate.activate(); err != nil {
		t.Fatal(err)
	}
	// Once dispatch starts, ordinary production writes are not held behind
	// the browser admission gate and do not mutate its creation observation.
	response, err = http.Post(api.URL+"/projects/"+observed.ProjectID+"/hints", "application/json", bytes.NewBufferString(`{"content":"new evidence","creator":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusCreated || gate.snapshot().Failure != "" {
		t.Fatalf("gate altered post-start production writes: %d", response.StatusCode)
	}
}
