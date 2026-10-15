package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"pwnmesh/internal/board"
)

// Pause after flushing the first body chunk so this exercises a partially sent
// download crossing the real server socket's ordinary write deadline.
type slowInputResponseWriter struct {
	http.ResponseWriter
	started bool
}

func TestInputDownloadHeadReadsMetadataOnly(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	payload := []byte("original input")
	input := uploadFixture(t, f, "metadata.bin", payload, http.StatusCreated)
	// Reading the payload expression fails; metadata remains fully readable.
	// This detects payload reads even when HEAD discards their response body.
	if err := f.store.Do(context.Background(), func(tx *board.Tx) error {
		_, err := tx.Exec(`ALTER TABLE pwnmesh_input_files RENAME TO input_download_payload;
CREATE VIEW pwnmesh_input_files AS SELECT project_id,id,name,size,sha256,created_at,
 json_extract('invalid json','$') AS data FROM input_download_payload;`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, httptest.NewRequest(http.MethodHead, f.base()+"/inputs/"+input.ID, nil))
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") != strconv.Itoa(len(payload)) || w.Header().Get("X-Content-SHA256") != input.SHA256 {
		t.Fatalf("HEAD read input bytes or changed metadata: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
}

type observingInputResponseWriter struct {
	http.ResponseWriter
	beforeWrite func() error
}

func (w *observingInputResponseWriter) Write(data []byte) (int, error) {
	if w.beforeWrite != nil {
		observe := w.beforeWrite
		w.beforeWrite = nil
		if err := observe(); err != nil {
			return 0, err
		}
	}
	return w.ResponseWriter.Write(data)
}

func TestInputDownloadStagesRangeAndReleasesReadersBeforeSending(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	payload := bytes.Repeat([]byte{0, 255, '\r', '\n'}, 50000)
	input := uploadFixture(t, f, "range.bin", payload, http.StatusCreated)
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	w := httptest.NewRecorder()
	observed := false
	output := &observingInputResponseWriter{ResponseWriter: w, beforeWrite: func() error {
		observed = true
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("staging pathname could leak after process exit: %v %v", entries, err)
		}
		// Hold every read connection at once. A response must not retain one
		// of these slots while its client is blocked receiving body bytes.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		entered, release, done := make(chan struct{}, 4), make(chan struct{}), make(chan error, 4)
		for i := 0; i < 4; i++ {
			go func() {
				done <- f.store.View(ctx, func(*board.Tx) error {
					entered <- struct{}{}
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
		}
		for i := 0; i < 4; i++ {
			select {
			case <-entered:
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		close(release)
		for i := 0; i < 4; i++ {
			err = errors.Join(err, <-done)
		}
		if err != nil {
			t.Errorf("network response retained a database reader: %v", err)
			return err
		}
		// Deleting the database object after staging must not truncate the
		// response. The request owns its verified bytes until transfer ends.
		return f.store.Do(ctx, func(tx *board.Tx) error { return tx.DeleteProject(f.project) })
	}}
	r := httptest.NewRequest(http.MethodGet, f.base()+"/inputs/"+input.ID, nil)
	r.Header.Set("Range", "bytes=65530-65545")
	f.handler.ServeHTTP(output, r)
	if !observed || w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), payload[65530:65546]) || w.Header().Get("Content-Range") != "bytes 65530-65545/200000" {
		t.Fatalf("staged range changed bytes or metadata: %d %v %v", w.Code, w.Header(), w.Body.Bytes())
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("successful download retained staging files: %v %v", entries, err)
	}
}

func TestInputDownloadRemovesStagingOnFailure(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	input := uploadFixture(t, f, "fail.bin", []byte{0, 255, 1}, http.StatusCreated)
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	for _, failure := range []string{"network", "range", "digest", "missing"} {
		t.Run(failure, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, f.base()+"/inputs/"+input.ID, nil)
			var output http.ResponseWriter = w
			wantStatus := http.StatusOK
			switch failure {
			case "network":
				output = &observingInputResponseWriter{ResponseWriter: w, beforeWrite: func() error { return errors.New("client disconnected") }}
			case "range":
				r.Header.Set("Range", "bytes=4-5")
				wantStatus = http.StatusRequestedRangeNotSatisfiable
			case "digest":
				if err := f.store.Do(context.Background(), func(tx *board.Tx) error {
					_, err := tx.Exec("UPDATE pwnmesh_input_files SET data=X'00FE01' WHERE project_id=? AND id=?", f.project, input.ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				wantStatus = http.StatusInternalServerError
			case "missing":
				r = httptest.NewRequest(http.MethodGet, f.base()+"/inputs/missing", nil)
				wantStatus = http.StatusNotFound
			}
			f.handler.ServeHTTP(output, r)
			if w.Code != wantStatus {
				t.Fatalf("download failure returned HTTP %d, want %d", w.Code, wantStatus)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("failed download retained staging files: %v %v", entries, err)
			}
		})
	}
	// HEAD needs neither a content read nor writable staging storage.
	t.Setenv("TMPDIR", filepath.Join(dir, "missing-directory"))
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, httptest.NewRequest(http.MethodHead, f.base()+"/inputs/"+input.ID, nil))
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("HEAD required staging storage: %d", w.Code)
	}
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, f.base()+"/inputs/"+input.ID, nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("staging creation failure returned HTTP %d", w.Code)
	}
}

func (w *slowInputResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *slowInputResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if !w.started && err == nil {
		w.started = true
		if err = http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
			return n, err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return n, err
}

func TestInputDownloadOutlivesOrdinaryWriteDeadline(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	payload := bytes.Repeat([]byte("PK\x00\xffAPK evidence\r\n"), 8192)
	input := uploadFixture(t, f, "slow.apk", payload, http.StatusCreated)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.handler.ServeHTTP(&slowInputResponseWriter{ResponseWriter: w}, r)
	}))
	srv.Config.WriteTimeout = 100 * time.Millisecond
	srv.Start()
	defer srv.Close()

	client := srv.Client()
	client.Timeout = 5 * time.Second
	response, err := client.Get(srv.URL + f.base() + "/inputs/" + input.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength != int64(len(payload)) {
		t.Fatalf("download HTTP %d, content length %d; want 200, %d", response.StatusCode, response.ContentLength, len(payload))
	}
	downloaded, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("download truncated after %d/%d bytes: %v", len(downloaded), len(payload), err)
	}
	sum := sha256.Sum256(downloaded)
	if !bytes.Equal(downloaded, payload) || response.Header.Get("X-Content-SHA256") != hex.EncodeToString(sum[:]) {
		t.Fatal("slow download did not retain its complete original bytes and checksum")
	}
}
