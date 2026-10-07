//go:build linux

package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/dispatcher"
	"pwnmesh/internal/server"
	"pwnmesh/internal/worker"
)

// Opt in with an existing worker image. This test never builds or deletes an
// image, starts a model, or touches containers outside its random namespace.
func TestDockerUploadedInputsRoundTrip(t *testing.T) {
	image := os.Getenv("PWNMESH_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set PWNMESH_DOCKER_TEST_IMAGE to an existing worker image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, err := board.Open(filepath.Join(t.TempDir(), "inputs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var downloads atomic.Int32
	handler := server.New(store)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/inputs/") {
			downloads.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)
	boardClient := &dispatcher.Client{Base: api.URL, HTTP: api.Client()}
	var graph board.Graph
	if err = boardClient.Do(ctx, http.MethodPost, "/projects", map[string]any{
		"title": "Docker input acceptance", "origin": "Synthetic binary fixture",
		"goal": "Verify immutable input transfer without a model", "start_paused": true,
	}, &graph, nil); err != nil {
		t.Fatal(err)
	}
	if graph.Project.Status != "stopped" {
		t.Fatalf("upload project started prematurely: %s", graph.Project.Status)
	}
	payload := make([]byte, 65539)
	for index := range payload {
		payload[index] = byte(index)
	}
	base := "/projects/" + graph.Project.ID
	upload := func() board.InputFile {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, err := form.CreateFormFile("file", "客户端 fixture.apk")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = part.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err = form.Close(); err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, api.URL+base+"/inputs", &body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", form.FormDataContentType())
		response, err := api.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusCreated {
			detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			t.Fatalf("upload HTTP %d: %s", response.StatusCode, detail)
		}
		var input board.InputFile
		if err = json.NewDecoder(response.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		return input
	}
	input := upload()
	sum := sha256.Sum256(payload)
	if !input.Valid() || input.Size != int64(len(payload)) || input.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("invalid persisted input metadata: %+v", input)
	}
	if repeated := upload(); repeated != input {
		t.Fatal("retry upload changed immutable metadata")
	}
	var suffix [8]byte
	if _, err = rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	socket := os.Getenv("PWNMESH_DOCKER_TEST_SOCKET")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	c := New(config.Container{Image: image, Socket: socket, Network: "none", Namespace: "pwn-input-test-" + hex.EncodeToString(suffix[:])})
	t.Cleanup(c.Close)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		if err := c.Cleanup(cleanupCtx, graph.Project.ID, "deleted"); err != nil {
			t.Errorf("remove own test container: %v", err)
		}
	})
	c.SetInputReader(boardClient.OpenInput)
	name, err := c.ensure(ctx, graph.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	job := worker.Job{Graph: graph, InputFiles: []board.InputFile{input}}
	readStaged := func() []byte {
		t.Helper()
		var output bytes.Buffer
		if _, err := c.exec(ctx, name, []string{"cat", "--", input.Path}, nil, &output); err != nil {
			t.Fatal(err)
		}
		return output.Bytes()
	}
	for pass := 0; pass < 2; pass++ {
		if err = c.stageInputs(ctx, name, job); err != nil {
			t.Fatalf("stage pass %d: %v", pass, err)
		}
		if !bytes.Equal(readStaged(), payload) {
			t.Fatalf("stage pass %d changed binary payload", pass)
		}
		if downloads.Load() != 1 {
			t.Fatalf("stage pass %d downloaded %d times, want 1", pass, downloads.Load())
		}
	}
	// Workers may alter their local copies; the next staging restores the
	// original input without changing the board's immutable evidence.
	if _, err = c.exec(ctx, name, []string{"sh", "-c", `chmod u+w "$1" && printf changed > "$1"`, "input-test", input.Path}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readStaged(), []byte("changed")) {
		t.Fatal("test did not modify its local input copy")
	}
	if err = c.stageInputs(ctx, name, job); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 2 || !bytes.Equal(readStaged(), payload) {
		t.Fatal("staging did not redownload and repair the changed local copy")
	}
	for _, bad := range [][]byte{payload[:2], append(append([]byte{}, payload...), 1), bytes.Repeat([]byte{'x'}, len(payload))} {
		if err = c.archiveInput(ctx, name, input, io.NopCloser(bytes.NewReader(bad))); err == nil {
			t.Fatal("corrupt download was installed")
		}
		if !bytes.Equal(readStaged(), payload) {
			t.Fatal("failed download replaced the original input")
		}
		if _, err = c.exec(ctx, name, []string{"sh", "-c", `test ! -e "$1" && test ! -L "$1"`, "input-test", input.Path + ".partial"}, nil, io.Discard); err != nil {
			t.Fatalf("failed download left a partial input: %v", err)
		}
	}
	var files []board.InputFile
	if err = boardClient.Do(ctx, http.MethodGet, base+"/inputs", nil, &files, nil); err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != input {
		t.Fatalf("staging changed persisted input metadata: %+v", files)
	}
}
