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
	for _, suffix := range []string{"/state", "/state/events", "/rounds", "/inputs", "/executions", "/identity"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		request := httptest.NewRequest(http.MethodGet, f.base()+suffix, nil).WithContext(ctx)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		cancel()
		if response.Code != http.StatusOK {
			t.Fatalf("observational %s waited for the writer: %d %s", suffix, response.Code, response.Body.String())
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
