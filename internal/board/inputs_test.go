package board

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestInputUploadHandledFailureRollsBackFileAndInstruction(t *testing.T) {
	f := newOrchestrationFixture(t)
	before, _ := json.Marshal(f.state())
	f.do(func(tx *Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER fail_upload_event BEFORE INSERT ON pwnmesh_state_events BEGIN SELECT RAISE(ABORT,'upload event failed'); END`)
		return err
	})
	f.do(func(tx *Tx) error {
		if _, err := tx.AddInput("p", "capture.bin", []byte{0, 255, 13, 10}); err == nil || !strings.Contains(err.Error(), "upload event failed") {
			t.Fatalf("upload did not reach the injected event failure: %v", err)
		}
		// A caller may handle a rejected upload and commit unrelated work.
		_, err := tx.SetProjectTitle("p", "continued transaction")
		return err
	})
	after := f.state()
	if after.Graph.Project.Title != "continued transaction" {
		t.Fatal("handled upload failure rolled back unrelated work")
	}
	after.Graph.Project.Title = "orchestration"
	raw, _ := json.Marshal(after)
	if string(raw) != string(before) {
		t.Fatal("handled upload failure retained an instruction or state revision")
	}
	f.do(func(tx *Tx) error {
		for _, table := range []string{"pwnmesh_input_files", "pwnmesh_state_events", "scoped_counters"} {
			var count int
			if err := tx.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE project_id='p'").Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("handled upload failure retained %d rows in %s", count, table)
			}
		}
		if _, err := tx.Exec("DROP TRIGGER fail_upload_event"); err != nil {
			return err
		}
		_, err := tx.AddInput("p", "capture.bin", []byte{0, 255, 13, 10})
		return err
	})
	if state := f.state(); len(state.Graph.Hints) != 1 || state.Revision != 1 {
		t.Fatalf("retry did not create exactly one instruction and event: %+v", state)
	}
}

func TestInputReceiptReplayKeepsEmptyShapeAndBoardPolicy(t *testing.T) {
	f := newOrchestrationFixture(t)
	f.do(func(tx *Tx) error {
		for _, tc := range []struct {
			key    string
			inputs []InputFile
		}{{"null", nil}, {"empty", []InputFile{}}, {"metadata", []InputFile{{ID: "original", Name: "capture.bin", Path: "/original/execution/path", Size: 2}}}} {
			if err := tx.SaveInputBatchReceipt("p", tc.key, "digest", tc.inputs); err != nil {
				return err
			}
			got, found, err := tx.InputBatchReceipt("p", tc.key, "digest")
			if err != nil || !found || !reflect.DeepEqual(got, tc.inputs) {
				t.Fatalf("receipt shape or path changed: %+v, %t, %v", got, found, err)
			}
			var api *APIError
			if _, found, err := tx.InputBatchReceipt("p", tc.key, "different"); !errors.As(err, &api) || api.Status != 409 || found {
				t.Fatalf("receipt mismatch policy changed: %t, %v", found, err)
			}
		}
		if err := tx.SaveInputBatchReceipt("missing", "", "ignored", nil); err != nil {
			return err
		}
		for _, lookup := range []struct{ project, key string }{{"missing", "null"}, {"p", "missing"}, {"p", ""}} {
			got, found, err := tx.InputBatchReceipt(lookup.project, lookup.key, "digest")
			if err != nil || found || got != nil {
				t.Fatalf("absent receipt changed: %+v, %t, %v", got, found, err)
			}
		}
		files, err := tx.InputFiles("p")
		if err != nil || files == nil || len(files) != 0 {
			t.Fatalf("empty file list changed: %+v, %v", files, err)
		}
		var api *APIError
		if _, err := tx.InputFiles("missing"); !errors.As(err, &api) || api.Status != 404 {
			t.Fatalf("missing project policy changed: %v", err)
		}
		if _, data, err := tx.InputData("p", "missing"); !errors.As(err, &api) || api.Status != 404 || data != nil {
			t.Fatalf("missing file policy changed: %v, %v", data, err)
		}
		return nil
	})
}
