package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/config"
	"pwnmesh/internal/protocol"
	"pwnmesh/internal/server"
)

var errTestOwnerChanged = errors.New("runner installation changed")

type ownershipRunner struct {
	owner                    string
	projects, cleanups, runs int
	cleanupError             error
}

func (r *ownershipRunner) BindOwner(owner string) error {
	if r.owner != "" && r.owner != owner {
		return errTestOwnerChanged
	}
	r.owner = owner
	return nil
}

func (r *ownershipRunner) Projects(context.Context) ([]string, error) {
	r.projects++
	return []string{"active-project"}, nil
}

func (r *ownershipRunner) Cleanup(context.Context, string, string) error {
	r.cleanups++
	return r.cleanupError
}

func (r *ownershipRunner) Run(context.Context, config.Worker, protocol.Job) (protocol.Result, error) {
	r.runs++
	return protocol.Result{}, nil
}

func (r *ownershipRunner) assertUntouched(t *testing.T) {
	t.Helper()
	if r.projects != 0 || r.cleanups != 0 || r.runs != 0 {
		t.Fatalf("runner used without dispatcher ownership: projects=%d cleanups=%d runs=%d", r.projects, r.cleanups, r.runs)
	}
}

func TestDispatcherOwnershipRejectsConcurrentStartup(t *testing.T) {
	store, err := openFixtureStore(t, filepath.Join(t.TempDir(), "ownership.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var deployments atomic.Int32
	handler := server.New(store)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/deployment" {
			deployments.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer httpServer.Close()
	cfg := config.Config{
		Server:    httpServer.URL,
		Container: config.Container{Namespace: "ownership-test"},
		Runtime:   config.Runtime{Interval: 1, MaxWorkers: 1, MaxProjects: 1, MaxProjectWorkers: 1, HealthMode: "disabled", HealthTimeout: 1},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	firstRunner := &ownershipRunner{}
	first := New(cfg, firstRunner)
	_, release, err := first.acquireDispatcher(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	secondRunner := &ownershipRunner{}
	before := deployments.Load()
	err = New(cfg, secondRunner).Run(ctx)
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Status != http.StatusConflict {
		t.Fatalf("second dispatcher startup: got %v, want ownership conflict", err)
	}
	if reads := deployments.Load() - before; reads != 1 {
		t.Fatalf("dispatcher startup read its installation %d times, want once before claiming", reads)
	}
	secondRunner.assertUntouched(t)
	if firstRunner.owner == "" || secondRunner.owner != firstRunner.owner {
		t.Fatalf("dispatchers did not bind the same installation: first=%q second=%q", firstRunner.owner, secondRunner.owner)
	}
}

func TestDispatcherOwnershipRejectsInstallationChangeBeforeClaim(t *testing.T) {
	const previous = "00000000000000000000000000000001"
	const current = "00000000000000000000000000000002"
	var posts, deployments atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		if r.Method == http.MethodGet && r.URL.Path == "/deployment" {
			deployments.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"installation_id":%q}`, current)
			return
		}
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer httpServer.Close()
	runner := &ownershipRunner{owner: previous}
	scheduler := New(config.Config{Server: httpServer.URL, Container: config.Container{Namespace: "ownership-test"}}, runner)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, release, err := scheduler.acquireDispatcher(ctx)
	if release != nil {
		defer release()
	}
	if !errors.Is(err, errTestOwnerChanged) {
		t.Fatalf("changed installation: got %v, want runner ownership rejection", err)
	}
	if deployments.Load() != 1 || posts.Load() != 0 {
		t.Fatalf("changed installation reached lease mutation: deployments=%d posts=%d", deployments.Load(), posts.Load())
	}
	if runner.owner != previous {
		t.Fatalf("runner changed installation from %q to %q", previous, runner.owner)
	}
	runner.assertUntouched(t)
}

func TestDispatcherStartupFailuresReleaseOwnershipBeforeScheduling(t *testing.T) {
	cleanupError := errors.New("startup container cleanup failed")
	for _, tc := range []struct {
		name                       string
		intentTimeout              int
		reasonTimeout              int
		cleanupError               error
		wantProjects, wantCleanups int
	}{
		{name: "intent lease too short", intentTimeout: 1, reasonTimeout: 30},
		{name: "reason lease too short", intentTimeout: 30, reasonTimeout: 1},
		{name: "cleanup failed", intentTimeout: 30, reasonTimeout: 30, cleanupError: cleanupError, wantProjects: 1, wantCleanups: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const installation = "00000000000000000000000000000001"
			var requests []string
			var requestsMu sync.Mutex
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestsMu.Lock()
				requests = append(requests, r.Method+" "+r.URL.Path)
				requestsMu.Unlock()
				switch r.Method + " " + r.URL.Path {
				case "GET /deployment":
					fmt.Fprintf(w, `{"installation_id":%q}`, installation)
				case "GET /settings":
					fmt.Fprintf(w, `{"intent_timeout":%d,"reason_timeout":%d}`, tc.intentTimeout, tc.reasonTimeout)
				case "POST /dispatchers/pwnmesh/claim", "POST /dispatchers/pwnmesh/release":
					w.WriteHeader(http.StatusNoContent)
				default:
					http.Error(w, "startup failure reached scheduling", http.StatusInternalServerError)
				}
			}))
			defer api.Close()
			runner := &ownershipRunner{cleanupError: tc.cleanupError}
			scheduler := New(config.Config{Server: api.URL, Runtime: config.Runtime{Interval: 1, HealthMode: "disabled"}}, runner)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := scheduler.Run(ctx)
			if tc.cleanupError != nil {
				if !errors.Is(err, tc.cleanupError) {
					t.Fatalf("startup returned %v, want cleanup error", err)
				}
			} else if err == nil || err.Error() != "heartbeat interval must be shorter than each server lease timeout" {
				t.Fatalf("startup returned %v, want invalid lease interval", err)
			}
			wantRequests := []string{"GET /deployment", "POST /dispatchers/pwnmesh/claim", "GET /settings", "POST /dispatchers/pwnmesh/release"}
			requestsMu.Lock()
			defer requestsMu.Unlock()
			if !slices.Equal(requests, wantRequests) {
				t.Fatalf("startup requests = %v, want %v", requests, wantRequests)
			}
			if runner.owner != installation || runner.projects != tc.wantProjects || runner.cleanups != tc.wantCleanups || runner.runs != 0 {
				t.Fatalf("startup runner state: owner=%q projects=%d cleanups=%d runs=%d", runner.owner, runner.projects, runner.cleanups, runner.runs)
			}
		})
	}
}
