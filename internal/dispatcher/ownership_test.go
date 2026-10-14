package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/server"
	"pwnmesh/internal/protocol"
)

var errTestOwnerChanged = errors.New("runner installation changed")

type ownershipRunner struct {
	owner                    string
	projects, cleanups, runs int
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
	return nil
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
	store, err := board.Open(filepath.Join(t.TempDir(), "ownership.db"))
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
	var protocol *ProtocolError
	if !errors.As(err, &protocol) || protocol.Status != http.StatusConflict {
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
