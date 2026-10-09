package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/board"
)

type batchFixtureFile struct {
	name string
	data []byte
}

func inputBatchBody(t *testing.T, files []batchFixtureFile, content string) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, file := range files {
		part, err := mw.CreateFormFile("file", file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = part.Write(file.data); err != nil {
			t.Fatal(err)
		}
	}
	if content != "" {
		if err := mw.WriteField("content", content); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), mw.FormDataContentType()
}

func inputBatchRequest(t *testing.T, f *executionProtocolFixture, body []byte, contentType string, status int) []board.InputFile {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, f.base()+"/inputs/batch", bytes.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("batch upload HTTP %d want %d: %s", w.Code, status, w.Body.String())
	}
	var inputs []board.InputFile
	if status == http.StatusCreated {
		if err := json.Unmarshal(w.Body.Bytes(), &inputs); err != nil {
			t.Fatal(err)
		}
	}
	return inputs
}

func isolatedInputBatchTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, dir)
	}
	return dir
}

func assertInputBatchTempEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("batch left temporary files: %v, %v", entries, err)
	}
}

type gatedBatchReader struct {
	prefix, suffix  *bytes.Reader
	waiting, resume chan struct{}
	once            sync.Once
}

func (r *gatedBatchReader) Read(p []byte) (int, error) {
	if r.prefix.Len() > 0 {
		return r.prefix.Read(p)
	}
	r.once.Do(func() { close(r.waiting) })
	<-r.resume
	return r.suffix.Read(p)
}

func TestInputBatchWaitsForAllFilesAndFreezesAccompanyingContent(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	dir := isolatedInputBatchTemp(t)
	content := "Only inspect the authorized test account; preserve both client builds."
	files := []batchFixtureFile{{"first.apk", []byte("FIRST_APK")}, {"second.apk", []byte("SECOND_APK")}}
	body, contentType := inputBatchBody(t, files, content)
	split := bytes.Index(body, files[1].data)
	reader := &gatedBatchReader{prefix: bytes.NewReader(body[:split]), suffix: bytes.NewReader(body[split:]), waiting: make(chan struct{}), resume: make(chan struct{})}
	var resume sync.Once
	unblock := func() { resume.Do(func() { close(reader.resume) }) }
	defer unblock()
	before, beforeEvents := f.state(), storedEvents(f)
	r := httptest.NewRequest(http.MethodPost, f.base()+"/inputs/batch", reader)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { f.handler.ServeHTTP(w, r); close(done) }()
	select {
	case <-reader.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not reach the blocked second file")
	}
	if state := f.state(); !reflect.DeepEqual(state, before) || !reflect.DeepEqual(storedEvents(f), beforeEvents) {
		t.Fatal("partially received supplement changed scheduling input")
	}
	var pending []board.InputFile
	f.request("GET", f.base()+"/inputs", nil, false, http.StatusOK, &pending)
	if len(pending) != 0 {
		t.Fatal("first file became visible while the second file was still uploading")
	}
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("complete batch did not finish")
	}
	if w.Code != http.StatusCreated {
		t.Fatalf("batch upload HTTP %d: %s", w.Code, w.Body.String())
	}
	var inputs []board.InputFile
	if err := json.Unmarshal(w.Body.Bytes(), &inputs); err != nil {
		t.Fatal(err)
	}
	_, job := prepareSnapshot(t, f, snapshotTemplate(f, "reason"))
	if !reflect.DeepEqual(job.InputFiles, inputs) && !(len(job.InputFiles) == 2 && len(inputs) == 2 && job.InputFiles[0] == inputs[1] && job.InputFiles[1] == inputs[0]) {
		t.Fatalf("execution did not freeze both files: %+v", job.InputFiles)
	}
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		state, err := tx.ReadInputSnapshot(f.project, job.InputSnapshot.ID)
		if err != nil {
			return err
		}
		if len(state.Graph.Hints) != 3 || state.Graph.Hints[2].Content != content {
			t.Fatalf("snapshot omitted accompanying instructions: %+v", state.Graph.Hints)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertInputBatchTempEmpty(t, dir)
}

func TestInputBatchInvalidRequestsLeaveNoFilesHintsOrTemporaryFiles(t *testing.T) {
	for _, mode := range []string{"invalid second filename", "truncated", "unknown field", "duplicate content", "long content", "invalid UTF-8", "no files", "too many files", "cross origin", "worker"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := newSnapshotHTTPFixture(t)
			dir := isolatedInputBatchTemp(t)
			before, beforeEvents := f.state(), storedEvents(f)
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			if mode != "no files" {
				part, _ := mw.CreateFormFile("file", "first.apk")
				_, _ = part.Write([]byte("first"))
			}
			switch mode {
			case "invalid second filename":
				_, _ = mw.CreateFormFile("file", "../second.apk")
			case "unknown field":
				_ = mw.WriteField("unexpected", "value")
			case "duplicate content":
				_ = mw.WriteField("content", "first instruction")
				_ = mw.WriteField("content", "second instruction")
			case "long content":
				_ = mw.WriteField("content", strings.Repeat("x", 32769))
			case "invalid UTF-8":
				_ = mw.WriteField("content", string([]byte{255}))
			case "too many files":
				for n := 0; n < board.MaxProjectInputs; n++ {
					_, _ = mw.CreateFormFile("file", fmt.Sprintf("file-%d.apk", n))
				}
			case "no files":
				_ = mw.WriteField("content", "instruction")
			}
			if mode != "truncated" {
				_ = mw.Close()
			}
			r := httptest.NewRequest(http.MethodPost, f.base()+"/inputs/batch", &body)
			r.Header.Set("Content-Type", mw.FormDataContentType())
			want := http.StatusUnprocessableEntity
			if mode == "too many files" {
				want = http.StatusRequestEntityTooLarge
			}
			if mode == "cross origin" {
				r.Header.Set("Origin", "https://outside.invalid")
				want = http.StatusForbidden
			}
			if mode == "worker" {
				r.Header.Set("X-PwnMesh-Run", "worker")
				want = http.StatusForbidden
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if w.Code != want {
				t.Fatalf("HTTP %d want %d: %s", w.Code, want, w.Body.String())
			}
			var inputs []board.InputFile
			f.request("GET", f.base()+"/inputs", nil, false, http.StatusOK, &inputs)
			if len(inputs) != 0 || !reflect.DeepEqual(f.state(), before) || !reflect.DeepEqual(storedEvents(f), beforeEvents) {
				t.Fatal("invalid batch left committed files, hints or events")
			}
			assertInputBatchTempEmpty(t, dir)
		})
	}
}

func TestInputBatchRollsBackFilesOnProjectQuotaAndContextAdmission(t *testing.T) {
	for _, mode := range []string{"quota", "context"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := newSnapshotHTTPFixture(t)
			dir := isolatedInputBatchTemp(t)
			content, want := "Accompanying instruction", http.StatusRequestEntityTooLarge
			if mode == "quota" {
				for n := 0; n < board.MaxProjectInputs-1; n++ {
					uploadFixture(t, f, fmt.Sprintf("existing-%d.apk", n), nil, http.StatusCreated)
				}
			} else {
				for n := 0; n < 3; n++ {
					f.request("POST", f.base()+"/hints", map[string]string{"content": strings.Repeat("x", 8192), "creator": "user"}, false, http.StatusCreated, nil)
				}
				content, want = strings.Repeat("y", 8192), http.StatusUnprocessableEntity
			}
			before, beforeEvents := f.state(), storedEvents(f)
			var beforeFiles []board.InputFile
			f.request("GET", f.base()+"/inputs", nil, false, http.StatusOK, &beforeFiles)
			body, contentType := inputBatchBody(t, []batchFixtureFile{{"new-1.apk", []byte("new1")}, {"new-2.apk", []byte("new2")}}, content)
			inputBatchRequest(t, f, body, contentType, want)
			var afterFiles []board.InputFile
			f.request("GET", f.base()+"/inputs", nil, false, http.StatusOK, &afterFiles)
			if !reflect.DeepEqual(afterFiles, beforeFiles) || !reflect.DeepEqual(f.state(), before) || !reflect.DeepEqual(storedEvents(f), beforeEvents) {
				t.Fatal("rejected batch partially persisted files, hints, counters or revisions")
			}
			assertInputBatchTempEmpty(t, dir)
		})
	}
}

func TestInputBatchPreservesEncodedBytesAndFileRetryDeduplication(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	dir := isolatedInputBatchTemp(t)
	data := []byte("POST /capture HTTP/1.1\r\n\r\nvalue=41=3D\r\n\x00\xff")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition":       {`form-data; name="file"; filename="客户端.http"`},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	_ = mw.Close()
	inputs := inputBatchRequest(t, f, body.Bytes(), mw.FormDataContentType(), http.StatusCreated)
	if len(inputs) != 1 || !inputs[0].Valid() {
		t.Fatalf("invalid input metadata: %+v", inputs)
	}
	again := inputBatchRequest(t, f, body.Bytes(), mw.FormDataContentType(), http.StatusCreated)
	if !reflect.DeepEqual(again, inputs) || len(f.state().Graph.Hints) != 1 {
		t.Fatal("retry duplicated file metadata or hint")
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, f.base()+"/inputs/"+inputs[0].ID, nil))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("batch upload changed original evidence bytes")
	}
	assertInputBatchTempEmpty(t, dir)
}

var _ io.Reader = (*gatedBatchReader)(nil)
