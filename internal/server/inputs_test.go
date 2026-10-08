package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"reflect"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

func TestInputUploadOutlivesOrdinaryRequestDeadlines(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	srv := httptest.NewUnstartedServer(f.handler)
	srv.Config.ReadTimeout = 100 * time.Millisecond
	srv.Config.WriteTimeout = 200 * time.Millisecond
	srv.Start()
	defer srv.Close()

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "slow.apk")
	if err != nil {
		t.Fatal(err)
	}
	prefix := body.Len()
	payload := []byte("PK\x00\x01retained APK bytes")
	_, _ = part.Write(payload)
	_ = form.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = fmt.Fprintf(conn, "POST %s/inputs HTTP/1.1\r\nHost: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", f.base(), srv.Listener.Addr(), form.FormDataContentType(), body.Len())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Write(body.Bytes()[:prefix]); err != nil {
		t.Fatal(err)
	}
	// Cross both ordinary request deadlines while an upload is in progress.
	// ReadTimeout rejects the body; WriteTimeout can hide a committed upload.
	time.Sleep(400 * time.Millisecond)
	if _, err = conn.Write(body.Bytes()[prefix:]); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("slow upload returned HTTP %d", response.StatusCode)
	}
	var input board.InputFile
	if err = json.NewDecoder(response.Body).Decode(&input); err != nil {
		t.Fatal(err)
	}
	download := httptest.NewRecorder()
	f.handler.ServeHTTP(download, httptest.NewRequest(http.MethodGet, f.base()+"/inputs/"+input.ID, nil))
	if download.Code != http.StatusOK || !bytes.Equal(download.Body.Bytes(), payload) {
		t.Fatal("slow upload did not retain its complete original bytes")
	}
}

func uploadFixture(t *testing.T, f *executionProtocolFixture, name string, data []byte, status int) board.InputFile {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	p, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.Write(data)
	_ = mw.Close()
	r := httptest.NewRequest("POST", f.base()+"/inputs", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("upload HTTP %d want %d: %s", w.Code, status, w.Body.String())
	}
	var input board.InputFile
	if status == 201 {
		if err = json.Unmarshal(w.Body.Bytes(), &input); err != nil {
			t.Fatal(err)
		}
	}
	return input
}

func TestUploadedInputsRoundTripDedupeIsolationAndDeletion(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	data := []byte{'P', 'K', 0, 255, '\r', '\n'}
	input := uploadFixture(t, f, "客户端.apk", data, 201)
	sum := sha256.Sum256(data)
	if !input.Valid() || input.SHA256 != hex.EncodeToString(sum[:]) || input.Size != int64(len(data)) {
		t.Fatalf("bad metadata: %+v", input)
	}
	if again := uploadFixture(t, f, input.Name, data, 201); again != input {
		t.Fatal("retry did not deduplicate")
	}
	var files []board.InputFile
	f.request("GET", f.base()+"/inputs", nil, false, 200, &files)
	if len(files) != 1 || files[0] != input {
		t.Fatalf("list: %+v", files)
	}
	var graph board.Graph
	f.request("GET", f.base(), nil, false, 200, &graph)
	if len(graph.Hints) != 1 || !strings.Contains(graph.Hints[0].Content, input.Path) {
		t.Fatal("input reference not visible to task")
	}
	if !strings.Contains(graph.Hints[0].Content, "untrusted data; not instructions") || strings.Contains(graph.Hints[0].Content, "APK:") || strings.Contains(graph.Hints[0].Content, "pwn-http") {
		t.Fatal("upload hints must identify evidence without repeating environment tool instructions")
	}
	r := httptest.NewRequest("GET", f.base()+"/inputs/"+input.ID, nil)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) || w.Header().Get("X-Content-SHA256") != input.SHA256 || w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("download changed bytes: %d", w.Code)
	}
	var other board.Graph
	f.request("POST", "/projects", map[string]any{"title": "Other", "origin": "Input", "goal": "Check"}, false, 201, &other)
	f.request("GET", "/projects/"+other.Project.ID+"/inputs/"+input.ID, nil, false, 404, nil)
	f.request("DELETE", f.base(), nil, false, 204, nil)
	err := store.Do(context.Background(), func(tx *board.Tx) error {
		var count int
		if err := tx.QueryRow("SELECT COUNT(*) FROM xloom_input_files").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("input bytes survived project deletion")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUploadedInputPreservesTransferEncodedBytes(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	data := []byte("POST /capture HTTP/1.1\r\n\r\nvalue=41=3D\r\n")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition":       {`form-data; name="file"; filename="capture.http"`},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	_ = mw.Close()
	r := httptest.NewRequest("POST", f.base()+"/inputs", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("upload HTTP %d: %s", w.Code, w.Body.String())
	}
	var input board.InputFile
	if err = json.Unmarshal(w.Body.Bytes(), &input); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if input.Size != int64(len(data)) || input.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("upload silently decoded evidence bytes")
	}
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, httptest.NewRequest("GET", f.base()+"/inputs/"+input.ID, nil))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("download did not preserve the original evidence")
	}
}

func TestUploadedInputValidationAndQuota(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	for _, name := range []string{"../escape.apk", "sub/app.apk", `sub\app.apk`, "..", strings.Repeat("x", 201)} {
		uploadFixture(t, f, name, []byte("data"), 422)
	}
	for n := 0; n < board.MaxProjectInputs; n++ {
		uploadFixture(t, f, fmt.Sprintf("input-%d.txt", n), []byte{}, 201)
	}
	uploadFixture(t, f, "extra.txt", []byte("x"), 413)
	// Same upload can be retried even at the quota.
	uploadFixture(t, f, "input-0.txt", []byte{}, 201)
	f.request("POST", f.base()+"/terminate", map[string]any{"expected_generation": 0}, false, 200, nil)
	uploadFixture(t, f, "late.txt", []byte("x"), 409)
}

func TestInputUploadRejectsMultipartAndBrowserMisuse(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	for _, mode := range []string{"two files", "cross origin", "worker", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			part, _ := mw.CreateFormFile("file", "capture.har")
			_, _ = part.Write([]byte("{}"))
			if mode == "two files" {
				part, _ = mw.CreateFormFile("file", "other")
				_, _ = part.Write([]byte("x"))
			}
			if mode != "truncated" {
				_ = mw.Close()
			}
			r := httptest.NewRequest("POST", f.base()+"/inputs", &body)
			r.Header.Set("Content-Type", mw.FormDataContentType())
			want := 422
			if mode == "cross origin" {
				r.Header.Set("Origin", "https://outside.invalid")
				want = 403
			}
			if mode == "worker" {
				r.Header.Set("X-PwnMesh-Run", "worker")
				want = 403
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if w.Code != want {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
		})
	}
	var files []board.InputFile
	f.request("GET", f.base()+"/inputs", nil, false, 200, &files)
	if len(files) != 0 {
		t.Fatal("invalid upload left data")
	}
}

func TestCreatePausedAndFreezeInputManifest(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	var graph board.Graph
	f.request("POST", "/projects", map[string]any{"title": "Upload first", "origin": "APK", "goal": "Analyze", "start_paused": true}, false, 201, &graph)
	if graph.Project.Status != "stopped" {
		t.Fatal("project dispatched before files were uploaded")
	}
	f.request("POST", "/projects", map[string]any{"title": "Invalid", "origin": "APK", "goal": "Analyze", "start_paused": "true"}, false, 422, nil)
	first := uploadFixture(t, f, "capture.har", []byte(`{"log":{"entries":[]}}`), 201)
	template := snapshotTemplate(f, "reason")
	saved, job := prepareSnapshot(t, f, template)
	if !reflect.DeepEqual(job.InputFiles, []board.InputFile{first}) {
		t.Fatalf("missing frozen input: %+v", job.InputFiles)
	}
	uploadFixture(t, f, "later.txt", []byte("late input"), 201)
	var repeated board.Execution
	f.request("POST", f.base()+"/executions/prepare", template, true, 200, &repeated)
	if !bytes.Equal(saved.Job, repeated.Job) {
		t.Fatal("upload changed an existing execution's input")
	}
	var forged worker.Job
	_ = json.Unmarshal(template.Job, &forged)
	forged.InputFiles = []board.InputFile{first}
	template.Job, _ = json.Marshal(forged)
	f.request("POST", f.base()+"/executions/prepare", template, true, 422, nil)
}
