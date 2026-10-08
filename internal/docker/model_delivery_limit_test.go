package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/modelrpc"
	"pwnmesh/internal/provider"
	"pwnmesh/internal/worker"
)

func TestModelBridgeBoundsPendingDeliveriesAndReadmitsAfterRelease(t *testing.T) {
	client := &http.Client{Transport: inputTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"role":"assistant","content":[{"type":"text","text":"fixture"}],"stop_reason":"end_turn"}`))}, nil
	})}
	arrived := make(chan string, 32)
	release := make(chan struct{})
	bridge := newModelBridge(context.Background(), &provider.Anthropic{Token: "fixture-token", Model: "fixture", MaxTokens: 32, Timeout: time.Second, Client: client}, worker.Job{RunID: "run"}, func(ctx context.Context, response modelrpc.Response) error {
		select {
		case arrived <- response.RequestID:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, func(error) {})
	defer bridge.close()
	request := func(n int) modelrpc.Request {
		return modelrpc.Request{RequestID: fmt.Sprintf("%032x", n), SessionID: "run"}
	}
	waitDelivery := func(id string) {
		t.Helper()
		select {
		case got := <-arrived:
			if got != id {
				t.Fatalf("delivery ID = %s, want %s", got, id)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("model did not reach its blocked delivery")
		}
	}
	// Every generation has completed; only response publication is blocked.
	for n := 1; n <= 32; n++ {
		r := request(n)
		if err := bridge.start(r); err != nil {
			t.Fatal(err)
		}
		waitDelivery(r.RequestID)
	}
	bridge.mu.Lock()
	active, pending := len(bridge.active), bridge.outstanding
	bridge.mu.Unlock()
	if active != 0 || pending != 32 {
		t.Fatalf("expected only 32 pending deliveries, got active=%d outstanding=%d", active, pending)
	}
	admitted := make(chan error, 1)
	go func() { admitted <- bridge.start(request(33)) }()
	select {
	case err := <-admitted:
		t.Fatalf("full delivery queue should wait, not finish admission: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	select {
	case release <- struct{}{}:
	case <-time.After(2 * time.Second):
		t.Fatal("no pending response could be released")
	}
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatalf("released delivery slot was not reusable: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("released delivery did not unblock the waiting request")
	}
	waitDelivery(request(33).RequestID)
	// Fill the queue again, then close while another request waits outside the
	// admission lock. It must be canceled without adding work after Wait starts.
	go func() { admitted <- bridge.start(request(34)) }()
	select {
	case err := <-admitted:
		t.Fatalf("second full queue did not wait: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	closed := make(chan struct{})
	go func() { bridge.close(); close(closed) }()
	select {
	case err := <-admitted:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("close did not cancel blocked admission: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close left a request waiting for a delivery slot")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not join admitted deliveries")
	}
	bridge.mu.Lock()
	active, pending = len(bridge.active), bridge.outstanding
	_, late := bridge.seen[request(34).RequestID]
	bridge.mu.Unlock()
	if active != 0 || pending != 0 || len(bridge.slots) != 0 || late {
		t.Fatalf("close left or admitted work: active=%d outstanding=%d slots=%d late=%v", active, pending, len(bridge.slots), late)
	}
}
