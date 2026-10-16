package board

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func stagedInputFixture(name, data string) InputBatchFile {
	sum := sha256.Sum256([]byte(data))
	return InputBatchFile{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Data: strings.NewReader(data)}
}

func TestInputBatchOwnsSemanticReceiptWithoutReadingReplays(t *testing.T) {
	f := newOrchestrationFixture(t)
	files := []InputBatchFile{stagedInputFixture("client.apk", "APK bytes"), stagedInputFixture("capture.har", "HAR bytes")}
	const content = "Inspect the authorized test account only."
	var first []InputFile
	f.do(func(tx *Tx) error {
		var err error
		first, err = tx.AddInputBatch("p", files, content, "supplement-1")
		if err != nil {
			return err
		}
		var digest string
		if err = tx.transactionQueries.QueryRow("SELECT request_sha256 FROM pwnmesh_input_batches WHERE project_id='p' AND idempotency_key='supplement-1'").Scan(&digest); err != nil {
			return err
		}
		if digest != "7bb2bfccc6fe05c336ea75961014e17509435d5e6858690f766510b808f9a6cf" {
			t.Fatalf("existing HTTP receipt identity changed: %s", digest)
		}
		if len(first) != 2 || first[0].Name != files[0].Name || first[1].Name != files[1].Name {
			t.Fatalf("batch order changed: %+v", first)
		}
		return nil
	})
	before := f.state()
	// Replay must work even after the original source descriptors have closed.
	for n := range files {
		files[n].Data = nil
	}
	f.do(func(tx *Tx) error {
		again, err := tx.AddInputBatch("p", files, " \n"+content+"\t", "supplement-1")
		if err != nil || !reflect.DeepEqual(first, again) {
			t.Fatalf("receipt replay consumed sources or changed metadata: %+v %v", again, err)
		}
		_, err = tx.AddInputBatch("p", files, "different instruction", "supplement-1")
		requireAPIStatus(t, err, 409)
		return nil
	})
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("replay or conflicting key changed project state")
	}
}

func TestInputBatchHandledFailureRollsBackWholeSupplement(t *testing.T) {
	for _, mode := range []string{"short source", "grown source", "wrong digest", "missing source", "receipt failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			before := f.state()
			f.do(func(tx *Tx) error {
				files := []InputBatchFile{stagedInputFixture("first.bin", "first"), stagedInputFixture("second.bin", "second")}
				switch mode {
				case "short source":
					files[1].Data = strings.NewReader("short")
				case "grown source":
					files[1].Data = strings.NewReader("second+")
				case "wrong digest":
					files[1].Data = strings.NewReader("WRONG!")
				case "missing source":
					files[1].Data = nil
				case "receipt failure":
					if _, err := tx.transactionQueries.Exec(`CREATE TRIGGER fail_batch_receipt BEFORE INSERT ON pwnmesh_input_batches BEGIN SELECT RAISE(ABORT,'receipt failed'); END`); err != nil {
						return err
					}
				}
				if receipt, err := tx.AddInputBatch("p", files, "Accompanying instruction", "failed-batch"); err == nil || receipt != nil {
					t.Fatalf("failed batch returned success or partial receipt: %+v %v", receipt, err)
				}
				// Keep the outer transaction successful to test the operation itself.
				_, err := tx.SetProjectTitle("p", "unrelated update")
				return err
			})
			after := f.state()
			if after.Graph.Project.Title != "unrelated update" {
				t.Fatal("batch failure rolled back unrelated work")
			}
			after.Graph.Project.Title = before.Graph.Project.Title
			if !reflect.DeepEqual(before, after) {
				t.Fatal("batch failure retained hints, revisions or other state")
			}
			f.do(func(tx *Tx) error {
				for _, table := range []string{"pwnmesh_input_files", "pwnmesh_input_batches", "pwnmesh_state_events", "scoped_counters"} {
					var count int
					if err := tx.transactionQueries.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE project_id='p'").Scan(&count); err != nil {
						return err
					}
					if count != 0 {
						t.Fatalf("failed batch retained %d rows in %s", count, table)
					}
				}
				if mode == "receipt failure" {
					if _, err := tx.transactionQueries.Exec("DROP TRIGGER fail_batch_receipt"); err != nil {
						return err
					}
				}
				_, err := tx.AddInputBatch("p", []InputBatchFile{stagedInputFixture("first.bin", "first"), stagedInputFixture("second.bin", "second")}, "Accompanying instruction", "failed-batch")
				return err
			})
			if state := f.state(); len(state.Graph.Hints) != 3 || state.Graph.Hints[0].ID != "h001" || state.Revision != 3 {
				t.Fatalf("retry did not restore the complete batch: %+v", state)
			}
		})
	}
}

func TestInputBatchValidatesSemanticLimitsBeforeReadingSources(t *testing.T) {
	for _, mode := range []string{"name", "negative size", "large file", "large batch", "digest", "key", "content", "empty"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			before := f.state()
			file := stagedInputFixture("input.bin", "data")
			file.Data = nil // A source error must not hide a semantic rejection.
			files, content, key, status := []InputBatchFile{file}, "instruction", "key", 422
			switch mode {
			case "name":
				files[0].Name = "../input.bin"
			case "negative size":
				files[0].Size = -1
			case "large file":
				files[0].Size, status = MaxInputBytes+1, 413
			case "large batch":
				files, status = []InputBatchFile{file, file, file}, 413
				for n := range files {
					files[n].Size = MaxInputBytes
				}
			case "digest":
				files[0].SHA256 = strings.Repeat("z", 64)
			case "key":
				key = "not a key"
			case "content":
				content = string([]byte{255})
			case "empty":
				files, content = nil, " \n"
			}
			f.do(func(tx *Tx) error {
				_, err := tx.AddInputBatch("p", files, content, key)
				requireAPIStatus(t, err, status)
				return nil
			})
			if !reflect.DeepEqual(before, f.state()) {
				t.Fatal("invalid batch changed state")
			}
		})
	}
}

func TestInputUploadHandledFailureRollsBackFileAndInstruction(t *testing.T) {
	f := newOrchestrationFixture(t)
	before, _ := json.Marshal(f.state())
	f.do(func(tx *Tx) error {
		_, err := tx.transactionQueries.Exec(`CREATE TRIGGER fail_upload_event BEFORE INSERT ON pwnmesh_state_events BEGIN SELECT RAISE(ABORT,'upload event failed'); END`)
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
			if err := tx.transactionQueries.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE project_id='p'").Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("handled upload failure retained %d rows in %s", count, table)
			}
		}
		if _, err := tx.transactionQueries.Exec("DROP TRIGGER fail_upload_event"); err != nil {
			return err
		}
		_, err := tx.AddInput("p", "capture.bin", []byte{0, 255, 13, 10})
		return err
	})
	if state := f.state(); len(state.Graph.Hints) != 1 || state.Revision != 1 {
		t.Fatalf("retry did not create exactly one instruction and event: %+v", state)
	}
}

func TestInputCopyChecksStoredBytesBeforePublication(t *testing.T) {
	f := newOrchestrationFixture(t)
	for _, tc := range []struct {
		name    string
		payload []byte
	}{{"empty.bin", []byte{}}, {"binary.bin", []byte{0, 255, '\r', '\n'}}} {
		f.do(func(tx *Tx) error {
			file, err := tx.AddInput("p", tc.name, tc.payload)
			if err != nil {
				return err
			}
			var copied bytes.Buffer
			metadata, err := tx.CopyInputData("p", file.ID, &copied)
			if err != nil || metadata != file || !bytes.Equal(copied.Bytes(), tc.payload) {
				t.Fatalf("verified copy changed metadata or bytes: %+v %v %v", metadata, copied.Bytes(), err)
			}
			if _, err := tx.transactionQueries.Exec("UPDATE pwnmesh_input_files SET sha256='corrupt' WHERE project_id='p' AND id=?", file.ID); err != nil {
				return err
			}
			if _, err := tx.CopyInputData("p", file.ID, io.Discard); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
				t.Fatalf("copy accepted a corrupt content digest: %v", err)
			}
			return nil
		})
	}
}

func TestInputReceiptReplayKeepsEmptyShapeAndBoardPolicy(t *testing.T) {
	f := newOrchestrationFixture(t)
	f.do(func(tx *Tx) error {
		for _, tc := range []struct {
			key    string
			inputs []InputFile
		}{{"null", nil}, {"empty", []InputFile{}}, {"metadata", []InputFile{{ID: "original", Name: "capture.bin", Path: "/original/execution/path", Size: 2}}}} {
			if err := tx.saveInputBatchReceipt("p", tc.key, "digest", tc.inputs); err != nil {
				return err
			}
			got, found, err := tx.inputBatchReceipt("p", tc.key, "digest")
			if err != nil || !found || !reflect.DeepEqual(got, tc.inputs) {
				t.Fatalf("receipt shape or path changed: %+v, %t, %v", got, found, err)
			}
			var api *APIError
			if _, found, err := tx.inputBatchReceipt("p", tc.key, "different"); !errors.As(err, &api) || api.Status != 409 || found {
				t.Fatalf("receipt mismatch policy changed: %t, %v", found, err)
			}
		}
		if err := tx.saveInputBatchReceipt("missing", "", "ignored", nil); err != nil {
			return err
		}
		for _, lookup := range []struct{ project, key string }{{"missing", "null"}, {"p", "missing"}, {"p", ""}} {
			got, found, err := tx.inputBatchReceipt(lookup.project, lookup.key, "digest")
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
		if _, err := tx.CopyInputData("p", "missing", io.Discard); !errors.As(err, &api) || api.Status != 404 {
			t.Fatalf("missing file policy changed: %v", err)
		}
		return nil
	})
}
