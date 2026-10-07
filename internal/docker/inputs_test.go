package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"pwnmesh/internal/board"
)

func inputFixture(data []byte) board.InputFile {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	name := "capture.har"
	return board.InputFile{ID: digest, Name: name, Size: int64(len(data)), SHA256: digest, Path: "/workspace/.pwnmesh/inputs/" + digest + "/" + name}
}

func TestInputArchivePreservesBytesAndRejectsCorruption(t *testing.T) {
	data := []byte{0, 255, '\r', '\n', 'P', 'K'}
	f := inputFixture(data)
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := writeInputTar(tw, f, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&archive)
	files := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		files++
		got, _ := io.ReadAll(tr)
		if !bytes.Equal(got, data) || h.Name != ".pwnmesh/inputs/"+f.ID+"/"+f.Name+".partial" || h.Mode != 0444 {
			t.Fatalf("bad archive entry: %+v", h)
		}
	}
	if files != 1 {
		t.Fatal("unexpected file count")
	}
	for _, bad := range [][]byte{data[:2], append(append([]byte{}, data...), 1), bytes.Repeat([]byte{'x'}, len(data))} {
		tw := tar.NewWriter(io.Discard)
		if err := writeInputTar(tw, f, bytes.NewReader(bad)); err == nil {
			t.Fatal("corrupt input accepted")
		}
	}
}

type blockedInput struct {
	started, closed chan struct{}
	once            sync.Once
}

func (b *blockedInput) Read([]byte) (int, error) {
	close(b.started)
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *blockedInput) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

type inputTestTransport func(*http.Request) (*http.Response, error)

func (f inputTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInputArchiveClosesBlockedDownloadOnEarlyEngineFailure(t *testing.T) {
	source := &blockedInput{started: make(chan struct{}), closed: make(chan struct{})}
	defer source.Close()
	cleanup := false
	c := &Client{http: &http.Client{Transport: inputTestTransport(func(r *http.Request) (*http.Response, error) {
		status, body := 200, ""
		switch {
		case strings.HasSuffix(r.URL.Path, "/archive"):
			defer r.Body.Close()
			tr := tar.NewReader(r.Body)
			for n := 0; n < 4; n++ {
				if _, err := tr.Next(); err != nil {
					return nil, err
				}
			}
			<-source.started
			status = 500
		case strings.HasSuffix(r.URL.Path, "/exec"):
			cleanup = true
			body = `{"Id":"cleanup"}`
		case strings.HasSuffix(r.URL.Path, "/json"):
			body = `{"Running":false,"ExitCode":0}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	done := make(chan error, 1)
	go func() { done <- c.archiveInput(context.Background(), "test", inputFixture([]byte("x")), source) }()
	select {
	case err := <-done:
		if err == nil || !cleanup {
			t.Fatalf("expected failed archive and partial-file cleanup: %v, cleanup=%v", err, cleanup)
		}
	case <-time.After(2 * time.Second):
		_ = source.Close()
		<-done
		t.Fatal("Engine failure waited for a blocked download instead of closing it")
	}
}
