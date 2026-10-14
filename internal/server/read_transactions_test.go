package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/board"
)

func TestObservationalHTTPReadsDoNotWaitForWriter(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- store.Do(context.Background(), func(tx *board.Tx) error {
			_, err := tx.Exec("UPDATE projects SET title='uncommitted title' WHERE id=?", f.project)
			close(started)
			<-release
			return err
		})
	}()
	<-started
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	paths := []string{"/health", "/settings"}
	for _, suffix := range []string{"/state", "/state/events", "/rounds", "/inputs", "/executions", "/executions/changes?generation=0", "/identity"} {
		paths = append(paths, f.base()+suffix)
	}
	for _, path := range paths {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		cancel()
		if response.Code != http.StatusOK {
			t.Fatalf("observational %s waited for the writer: %d %s", path, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "uncommitted title") {
			t.Fatal("observational endpoint exposed uncommitted writer data")
		}
	}
}

func TestObservationalStateDoesNotExpireLeasesButSchedulingDoes(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	old := store.Now().Add(-time.Hour).Format(time.RFC3339)
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		_, err := tx.Exec("UPDATE projects SET reason_worker='crashed',reason_trigger='initial',reason_started_at=?,reason_last_heartbeat_at=? WHERE id=?", old, old, f.project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var state board.State
	for _, path := range []string{"/health", "/settings"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("observational %s: %d %s", path, response.Code, response.Body.String())
		}
	}
	f.request("GET", f.base()+"/state", nil, false, http.StatusOK, &state)
	if state.Graph.Project.Reason == nil {
		t.Fatal("observational read performed lease housekeeping")
	}
	f.request("GET", f.base()+"/scheduling?protocol_version=1", nil, false, http.StatusOK, nil)
	f.request("GET", f.base()+"/state", nil, false, http.StatusOK, &state)
	if state.Graph.Project.Reason != nil {
		t.Fatal("scheduling no longer expires crashed leases")
	}
}

func TestSettingsWriteRetainsLeaseMaintenance(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	old := store.Now().Add(-time.Hour).Format(time.RFC3339)
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		_, err := tx.Exec("UPDATE projects SET reason_worker='crashed',reason_trigger='initial',reason_started_at=?,reason_last_heartbeat_at=? WHERE id=?", old, old, f.project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(`{"intent_timeout":120,"reason_timeout":180}`))
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("settings write: %d %s", response.Code, response.Body.String())
	}
	if err := store.View(context.Background(), func(tx *board.Tx) error {
		settings, err := tx.Settings()
		if err != nil {
			return err
		}
		if settings.IntentTimeout != 120 || settings.ReasonTimeout != 180 {
			t.Errorf("settings write lost: %+v", settings)
		}
		graph, err := tx.Load(f.project)
		if err == nil && graph.Project.Reason != nil {
			t.Error("settings write no longer expires crashed leases")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
