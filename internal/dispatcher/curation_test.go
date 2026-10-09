package dispatcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/worker"
)

func TestCurationCoalescingDrainsPromptlyAndCannotStarve(t *testing.T) {
	s := New(config.Config{}, nil)
	g := board.Graph{Project: board.Project{ID: "p"}}
	s.schedules["p"] = board.SchedulePage{Steps: []board.ScheduleStep{{ID: "i", Running: true}}}
	now := time.Unix(1000, 0)
	if !s.waitForCuration(g, 1, now) {
		t.Fatal("did not coalesce in-flight observation burst")
	}
	if !s.waitForCuration(g, 2, now.Add(10*time.Second)) {
		t.Fatal("new durable observation did not extend quiet period")
	}
	if s.waitForCuration(g, 3, now.Add(reasonMaxWait)) {
		t.Fatal("continuous producer starved curation")
	}
	s.schedules["p"] = board.SchedulePage{Steps: []board.ScheduleStep{{ID: "i", Status: "completed"}}}
	if s.waitForCuration(g, 4, now.Add(11*time.Second)) {
		t.Fatal("finished producers incurred quiet-period latency")
	}
}

func TestOrchestrationReservesControlCapacity(t *testing.T) {
	s := New(config.Config{Runtime: config.Runtime{MaxWorkers: 4, MaxProjectWorkers: 3}, Workers: []config.Worker{{Name: "all", TaskTypes: []string{"reason", "curate", "explore"}, MaxRunning: 3}}}, nil)
	s.running["one"] = &task{Job: worker.Job{Kind: "explore", Graph: board.Graph{Project: board.Project{ID: "p"}}}, Worker: config.Worker{Name: "all"}}
	s.running["two"] = &task{Job: worker.Job{Kind: "explore", Graph: board.Graph{Project: board.Project{ID: "p"}}}, Worker: config.Worker{Name: "all"}}
	if s.executionCapacity("p") || s.backendExecutionCapacity("all", 3) {
		t.Fatal("execution consumed reserved control capacity")
	}
	if !s.executionCapacity("q") {
		t.Fatal("another project cannot use its execution capacity")
	}
	s.running["three"] = &task{Job: worker.Job{Kind: "curate", Graph: board.Graph{Project: board.Project{ID: "p"}}}, Worker: config.Worker{Name: "all"}}
	if !s.executionCapacity("q") {
		t.Fatal("control task counted as an execution branch")
	}
	delete(s.running, "one")
	delete(s.running, "two")
	delete(s.running, "three")
	s.Config.Runtime.MaxWorkers, s.Config.Runtime.MaxProjectWorkers = 1, 1
	if !s.executionCapacity("p") || !s.backendExecutionCapacity("all", 1) {
		t.Fatal("serial installations must remain runnable")
	}
}

func TestOrchestrationCannotSelectLegacyMockBackend(t *testing.T) {
	s := New(config.Config{Workers: []config.Worker{{Name: "mock", Type: "mock", TaskTypes: []string{"reason", "curate", "explore"}, MaxRunning: 2}}}, nil)
	s.schedules["p"] = board.SchedulePage{Project: board.Project{ID: "p", OrchestrationVersion: 1}}
	for _, kind := range []string{"reason", "curate", "explore"} {
		if s.choose("p", kind) != nil {
			t.Fatalf("new mode selected legacy mock for %s", kind)
		}
	}
	s.schedules["p"] = board.SchedulePage{Project: board.Project{ID: "p"}}
	if s.choose("p", "reason") != nil {
		t.Fatal("legacy project selected a retired mock backend")
	}
}

func TestCurationReceiptLookupDistinguishesMissingFromTransportFailure(t *testing.T) {
	for _, tc := range []struct {
		status            int
		body              string
		committed, failed bool
	}{{200, `{"committed":true,"execution_status":"succeeded"}`, true, false}, {404, `{"detail":"not found"}`, false, false}, {503, `{"detail":"unavailable"}`, false, true}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/projects/p/state/curation/receipt" || r.Header.Get("X-PwnMesh-Lease") != "curate" || r.Header.Get("X-PwnMesh-Run") != "b@r" {
					t.Error("curation receipt lookup lost its run identity")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer api.Close()
			s := &Scheduler{Client: &Client{Base: api.URL}}
			job := &task{Job: worker.Job{Graph: board.Graph{Project: board.Project{ID: "p"}}}, Lease: Lease{Run: "b@r", Kind: "curate"}}
			committed, err := s.curationCommitted(context.Background(), job)
			if committed != tc.committed || (err != nil) != tc.failed {
				t.Fatalf("committed=%v err=%v", committed, err)
			}
		})
	}
}

func TestLegacyCurationReceiptRequiresSuccessfulExplicitSettlement(t *testing.T) {
	for _, tc := range []struct {
		name, execution, applied string
		status                   int
		wantApply                int32
		wantSuccess              bool
	}{
		{"success", "running", "succeeded", http.StatusOK, 1, true},
		{"forbidden", "running", "", http.StatusForbidden, 1, false},
		{"unavailable", "running", "", http.StatusServiceUnavailable, 1, false},
		{"rejected_after_read", "running", "rejected", http.StatusOK, 1, false},
		{"already_rejected", "rejected", "", http.StatusOK, 0, false},
		{"already_cancelled", "cancelled", "", http.StatusOK, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads, applies atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/projects/p/state/curation/receipt" {
					reads.Add(1)
					_, _ = w.Write([]byte(`{"committed":true,"execution_status":"` + tc.execution + `"}`))
					return
				}
				if r.Method != "POST" || r.URL.Path != "/projects/p/executions/r/apply" || r.Header.Get("X-PwnMesh-Lease") != "curate" || r.Header.Get("X-PwnMesh-Run") != "b@r" {
					t.Errorf("unexpected settlement request: %s %s", r.Method, r.URL.Path)
				}
				applies.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"status":"` + tc.applied + `","detail":"synthetic recovery boundary"}`))
			}))
			defer api.Close()
			s := &Scheduler{Client: &Client{Base: api.URL}}
			run := &task{Job: worker.Job{RunID: "r", Kind: "curate", Graph: board.Graph{Project: board.Project{ID: "p"}}}, Lease: Lease{Run: "b@r", Kind: "curate"}}
			committed, err := s.controlCommitted(context.Background(), run)
			if committed != tc.wantSuccess || (err == nil) != tc.wantSuccess || reads.Load() != 1 || applies.Load() != tc.wantApply {
				t.Fatalf("legacy receipt bypassed settlement: committed=%v err=%v reads=%d applies=%d", committed, err, reads.Load(), applies.Load())
			}
		})
	}
}
