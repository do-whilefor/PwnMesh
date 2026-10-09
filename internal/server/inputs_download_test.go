package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Pause after flushing the first body chunk so this exercises a partially sent
// download crossing the real server socket's ordinary write deadline.
type slowInputResponseWriter struct {
	http.ResponseWriter
	started bool
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
