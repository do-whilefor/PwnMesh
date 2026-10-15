package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/protocol"
)

func holdProjectWriter(t *testing.T, f *executionProtocolFixture) {
	t.Helper()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- f.store.Do(context.Background(), func(tx *board.Tx) error {
			_, err := tx.Exec("UPDATE projects SET title='uncommitted title' WHERE id=?", f.project)
			close(started)
			<-release
			return err
		})
	}()
	<-started
	t.Cleanup(func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
}

func TestObservationalHTTPReadsDoNotWaitForWriter(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	prepareSnapshot(t, f, snapshotTemplate(f, "explore"))
	holdProjectWriter(t, f)
	paths := []string{"/health", "/settings", "/projects", "/executions/pending?namespace=protocol-test"}
	for _, suffix := range []string{"", "/state", "/state/events", "/rounds", "/inputs", "/executions", "/executions/changes?generation=0", "/identity", "/export?format=yaml", "/export?format=timeline", "/executions/" + f.run + "?namespace=protocol-test", "/executions/" + f.run + "/identity?namespace=protocol-test"} {
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
	for _, endpoint := range []struct{ path, op string }{{"updates", "read_updates"}, {"traces/read", "read_trace_runs"}} {
		body, err := json.Marshal(protocol.GraphRequest{RequestID: strings.Repeat("a", 32), Op: endpoint.op})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		request := httptest.NewRequest(http.MethodPost, f.base()+"/executions/"+f.run+"/"+endpoint.path, bytes.NewReader(body)).WithContext(ctx)
		request.Header.Set("X-PwnMesh-Run", f.lease)
		request.Header.Set("X-PwnMesh-Lease", f.kind)
		request.Header.Set("X-PwnMesh-Intent", f.intent)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		cancel()
		if response.Code != http.StatusOK {
			t.Fatalf("observational %s waited for the writer: %d %s", endpoint.op, response.Code, response.Body.String())
		}
	}
}

func TestCurationReceiptReadDoesNotWaitForWriter(t *testing.T) {
	f := newCurationHTTPFixture(t)
	_, job := prepareCurator(f)
	f.action("curate", "committed", board.CuratePayload{ThroughRevision: job.InputSnapshot.Revision, Groups: []board.CurateGroup{}})
	holdProjectWriter(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, f.base()+"/state/curation/receipt", nil).WithContext(ctx)
	request.Header.Set("X-PwnMesh-Run", f.lease)
	request.Header.Set("X-PwnMesh-Lease", "curate")
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"committed":true`) {
		t.Fatalf("curation receipt waited for the writer: %d %s", response.Code, response.Body.String())
	}
}

func TestDecisionReceiptReadDoesNotWaitForWriter(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "committed"}[committed], func(t *testing.T) {
			f, _ := newSnapshotHTTPFixture(t)
			prepareSnapshot(t, f, snapshotTemplate(f, "reason"))
			if committed {
				f.decision("commit", f.batch(batchAction("step", "", `{"action":"add","from":["origin"],"description":"Inspect the fixture"}`)), http.StatusOK)
			}
			holdProjectWriter(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			request := httptest.NewRequest(http.MethodGet, f.base()+"/state/decisions/receipt", nil).WithContext(ctx)
			request.Header.Set("X-PwnMesh-Run", f.lease)
			request.Header.Set("X-PwnMesh-Lease", "reason")
			response := httptest.NewRecorder()
			f.handler.ServeHTTP(response, request)
			var receipt board.DecisionReceipt
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &receipt) != nil || receipt.Committed != committed {
				t.Fatalf("decision receipt waited for the writer or changed recovery state: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestDecisionReceiptReadRejectsExpiredAuthorityAndRetainsCommittedRecovery(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "committed"}[committed], func(t *testing.T) {
			f, _ := newSnapshotHTTPFixture(t)
			prepareSnapshot(t, f, snapshotTemplate(f, "reason"))
			var want board.DecisionReceipt
			status := http.StatusConflict
			if committed {
				want = f.decision("commit", f.batch(batchAction("step", "", `{"action":"add","from":["origin"],"description":"Inspect the fixture"}`)), http.StatusOK)
				status = http.StatusOK
			}
			later := f.store.Now().Add(time.Hour)
			f.store.Now = func() time.Time { return later }
			before := f.state()
			got := f.decisionReceipt(status)
			if committed && !reflect.DeepEqual(got, want) {
				t.Fatal("committed receipt changed after the original lease ended")
			}
			if !reflect.DeepEqual(before, f.state()) {
				t.Fatal("decision receipt performed lease maintenance")
			}
		})
	}
}

func TestLiveExecutionReadsRejectExpiredLeaseWithoutMutatingState(t *testing.T) {
	f, _ := prepareUpdateFixture(t, "snapshot")
	later := f.store.Now().Add(time.Hour)
	f.store.Now = func() time.Time { return later }
	before := f.state()
	for _, endpoint := range []struct{ path, op string }{{"updates", "read_updates"}, {"traces/read", "read_trace_runs"}} {
		read := protocol.GraphRequest{RequestID: strings.Repeat("a", 32), Op: endpoint.op}
		f.request("POST", f.base()+"/executions/"+f.run+"/"+endpoint.path, read, true, http.StatusConflict, nil)
		if !reflect.DeepEqual(before, f.state()) {
			t.Fatalf("%s expired or changed a stored lease", endpoint.op)
		}
	}
}

func TestObservationalHEADRequestsDoNotMutateProjects(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	before := f.state()
	for _, path := range []string{"/settings", "/projects", f.base()} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodHead, path, nil)
			response := httptest.NewRecorder()
			f.handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("HEAD returned %d: %s", response.Code, response.Body.String())
			}
		})
	}
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("HEAD changed the project")
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
	for _, path := range []string{"/health", "/settings", "/projects", f.base(), f.base() + "/export?format=timeline", "/executions/pending?namespace=protocol-test"} {
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

func TestSettingsRejectsCoercedTimeoutsWithoutChangingStorage(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	var before board.Settings
	if err := store.View(context.Background(), func(tx *board.Tx) error {
		var err error
		before, err = tx.Settings()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"intent_timeout", "reason_timeout"} {
		for _, value := range []string{`"15"`, `"15.0"`, `15.0`, `1.5e1`, `true`, `null`, `4`, `9223372036854775808`} {
			t.Run(field+"/"+value, func(t *testing.T) {
				body := `{"intent_timeout":120,"reason_timeout":180}`
				if field == "intent_timeout" {
					body = strings.Replace(body, "120", value, 1)
				} else {
					body = strings.Replace(body, "180", value, 1)
				}
				request := httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(body))
				response := httptest.NewRecorder()
				f.handler.ServeHTTP(response, request)
				if response.Code != http.StatusUnprocessableEntity {
					t.Fatalf("coerced timeout accepted: %d %s", response.Code, response.Body.String())
				}
				if err := store.View(context.Background(), func(tx *board.Tx) error {
					after, err := tx.Settings()
					if err == nil && after != before {
						t.Fatalf("invalid settings changed storage: before=%+v after=%+v", before, after)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
