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
	"path/filepath"
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

func inputBatchBody(t *testing.T, files []batchFixtureFile, content string, keys ...string) ([]byte, string) {
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
	for _, key := range keys {
		if err := mw.WriteField("idempotency_key", key); err != nil {
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
	body, contentType := inputBatchBody(t, files, " \n"+content+" \t")
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
	for _, mode := range []string{"invalid second filename", "truncated", "unknown field", "duplicate content", "long content", "invalid UTF-8", "empty input", "blank content", "too many files", "cross origin", "worker"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := newSnapshotHTTPFixture(t)
			dir := isolatedInputBatchTemp(t)
			before, beforeEvents := f.state(), storedEvents(f)
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			if mode != "empty input" && mode != "blank content" {
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
			case "blank content":
				_ = mw.WriteField("content", " \n\t")
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

func TestInputBatchKeyReplaysDurableReceiptWithoutDuplicatingInstructions(t *testing.T) {
	database := filepath.Join(t.TempDir(), "batch.db")
	f, _ := newSnapshotHTTPFixture(t, database)
	dir := isolatedInputBatchTemp(t)
	files := []batchFixtureFile{{"client.apk", []byte("APK bytes")}, {"capture.har", []byte("HAR bytes")}}
	const content = "Inspect the authorized test account only."
	body, contentType := inputBatchBody(t, files, content, "supplement-1")
	first := inputBatchRequest(t, f, body, contentType, http.StatusCreated)
	before, events := f.state(), storedEvents(f)

	// A lost acknowledgement can be retried after a server restart. Multipart
	// boundaries and insignificant content whitespace must not alter identity.
	reopened, err := board.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.store, f.handler = reopened, New(reopened)
	body, contentType = inputBatchBody(t, files, " \n"+content+"\t", "supplement-1")
	again := inputBatchRequest(t, f, body, contentType, http.StatusCreated)
	if !reflect.DeepEqual(first, again) || !reflect.DeepEqual(before, f.state()) || !reflect.DeepEqual(events, storedEvents(f)) {
		t.Fatal("same-key retry changed the original receipt, hints or revisions")
	}

	// A fresh submission may deliberately repeat instructions for existing files.
	body, contentType = inputBatchBody(t, files, content, "supplement-2")
	inputBatchRequest(t, f, body, contentType, http.StatusCreated)
	if len(f.state().Graph.Hints) != len(before.Graph.Hints)+1 {
		t.Fatal("a new submission was incorrectly deduplicated by its content")
	}
	f.request("POST", f.base()+"/restart", map[string]int{"expected_generation": 0}, false, http.StatusOK, nil)
	f.request("POST", f.base()+"/terminate", map[string]int{"expected_generation": 1}, false, http.StatusOK, nil)
	before, events = f.state(), storedEvents(f)
	body, contentType = inputBatchBody(t, files, content, "supplement-1")
	again = inputBatchRequest(t, f, body, contentType, http.StatusCreated)
	if !reflect.DeepEqual(first, again) || !reflect.DeepEqual(before, f.state()) || !reflect.DeepEqual(events, storedEvents(f)) {
		t.Fatal("project lifecycle changes lost the original upload acknowledgement")
	}
	f.request("DELETE", f.base(), nil, false, http.StatusNoContent, nil)
	if err := reopened.Do(context.Background(), func(tx *board.Tx) error {
		var count int
		if err := tx.QueryRow("SELECT COUNT(*) FROM pwnmesh_input_batches").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("project deletion retained upload receipts")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertInputBatchTempEmpty(t, dir)
}

func TestInputBatchTextOnlyRetryDoesNotDuplicateInstructions(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	const content = "Only inspect the authorized client."
	body, contentType := inputBatchBody(t, nil, content, "text-only-1")
	if inputs := inputBatchRequest(t, f, body, contentType, http.StatusCreated); inputs == nil || len(inputs) != 0 {
		t.Fatalf("text-only supplement returned file metadata: %+v", inputs)
	}
	before, events := f.state(), storedEvents(f)
	if len(before.Graph.Hints) != 1 || before.Graph.Hints[0].Content != content {
		t.Fatal("text-only supplement did not preserve its instruction")
	}
	inputBatchRequest(t, f, body, contentType, http.StatusCreated)
	if !reflect.DeepEqual(before, f.state()) || !reflect.DeepEqual(events, storedEvents(f)) {
		t.Fatal("retry duplicated text-only instructions or events")
	}
	body, contentType = inputBatchBody(t, nil, content, "text-only-2")
	inputBatchRequest(t, f, body, contentType, http.StatusCreated)
	if len(f.state().Graph.Hints) != len(before.Graph.Hints)+1 {
		t.Fatal("a deliberate new submission was deduplicated")
	}
}

func TestInputBatchTextOnlyCannotModifyUnsupportedProject(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	if err := f.store.Do(context.Background(), func(tx *board.Tx) error {
		_, err := tx.Exec("DELETE FROM pwnmesh_project_orchestration WHERE project_id=?", f.project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := func() (board.State, []board.StateEvent) {
		t.Helper()
		var state board.State
		var events []board.StateEvent
		if err := f.store.View(context.Background(), func(tx *board.Tx) error {
			var err error
			state, err = tx.State(f.project)
			if err == nil {
				events, err = tx.StateEvents(f.project, 0)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return state, events
	}
	before, events := snapshot()
	if before.Graph.Project.OrchestrationVersion != 0 {
		t.Fatal("fixture has a supported protocol")
	}
	f.request("GET", f.base()+"/state", nil, false, http.StatusConflict, nil)
	f.request("GET", f.base()+"/state/events", nil, false, http.StatusConflict, nil)
	body, contentType := inputBatchBody(t, nil, "Do not change retained historical evidence.", "historical-text")
	inputBatchRequest(t, f, body, contentType, http.StatusConflict)
	after, afterEvents := snapshot()
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(events, afterEvents) {
		t.Fatal("text-only upload changed an unsupported project")
	}
	if err := f.store.View(context.Background(), func(tx *board.Tx) error {
		var count int
		if err := tx.QueryRow("SELECT COUNT(*) FROM pwnmesh_input_batches WHERE project_id=?", f.project).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("rejected historical upload retained a receipt")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInputBatchKeyBindsFilesOrderNamesBytesAndContent(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	dir := isolatedInputBatchTemp(t)
	files := []batchFixtureFile{{"first.apk", []byte("first")}, {"second.apk", []byte("second")}}
	body, contentType := inputBatchBody(t, files, "Original instruction", "bound-request")
	inputBatchRequest(t, f, body, contentType, http.StatusCreated)
	before, events := f.state(), storedEvents(f)
	for _, mode := range []string{"bytes", "name", "order", "content"} {
		t.Run(mode, func(t *testing.T) {
			changed := append([]batchFixtureFile(nil), files...)
			content := "Original instruction"
			switch mode {
			case "bytes":
				changed[0].data = []byte("FIRST")
			case "name":
				changed[0].name = "renamed.apk"
			case "order":
				changed[0], changed[1] = changed[1], changed[0]
			case "content":
				content = "Different instruction"
			}
			body, contentType := inputBatchBody(t, changed, content, "bound-request")
			inputBatchRequest(t, f, body, contentType, http.StatusConflict)
			if !reflect.DeepEqual(before, f.state()) || !reflect.DeepEqual(events, storedEvents(f)) {
				t.Fatal("conflicting key changed project state")
			}
		})
	}
	assertInputBatchTempEmpty(t, dir)
}

func TestInputBatchRejectsMalformedOrDuplicateKeys(t *testing.T) {
	for _, keys := range [][]string{{""}, {"contains space"}, {"非ASCII"}, {strings.Repeat("k", 129)}, {"same", "same"}} {
		t.Run(fmt.Sprint(keys), func(t *testing.T) {
			f, _ := newSnapshotHTTPFixture(t)
			dir := isolatedInputBatchTemp(t)
			before, events := f.state(), storedEvents(f)
			body, contentType := inputBatchBody(t, []batchFixtureFile{{"fixture.apk", []byte("bytes")}}, "", keys...)
			inputBatchRequest(t, f, body, contentType, http.StatusUnprocessableEntity)
			if !reflect.DeepEqual(before, f.state()) || !reflect.DeepEqual(events, storedEvents(f)) {
				t.Fatal("invalid key changed project state")
			}
			assertInputBatchTempEmpty(t, dir)
		})
	}
}

func TestInputBatchFailedKeyCanBeRetried(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	dir := isolatedInputBatchTemp(t)
	for n := 0; n < 3; n++ {
		f.request("POST", f.base()+"/hints", map[string]string{"content": strings.Repeat("x", 8192), "creator": "user"}, false, http.StatusCreated, nil)
	}
	before, events := f.state(), storedEvents(f)
	files := []batchFixtureFile{{"fixture.apk", []byte("bytes")}}
	body, contentType := inputBatchBody(t, files, strings.Repeat("y", 8192), "retry-failed")
	inputBatchRequest(t, f, body, contentType, http.StatusUnprocessableEntity)
	if !reflect.DeepEqual(before, f.state()) || !reflect.DeepEqual(events, storedEvents(f)) {
		t.Fatal("failed batch retained partial state")
	}
	body, contentType = inputBatchBody(t, files, "Short instruction", "retry-failed")
	inputBatchRequest(t, f, body, contentType, http.StatusCreated)
	if len(f.state().Graph.Hints) != len(before.Graph.Hints)+2 {
		t.Fatal("failed batch reserved its key or left a partial file")
	}
	assertInputBatchTempEmpty(t, dir)
}

var _ io.Reader = (*gatedBatchReader)(nil)
