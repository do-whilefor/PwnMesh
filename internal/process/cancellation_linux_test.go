//go:build linux

package process

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestForceCancelConfirmsLockReleaseAndInvalidatesDelayedLaunch(t *testing.T) {
	dir := t.TempDir()
	token := strings.Repeat("a", 32)
	t.Setenv("PWNMESH_LAUNCH_TOKEN", token)
	if err := os.WriteFile(filepath.Join(dir, "launch-token"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckLaunch(dir); err != nil {
		t.Fatal(err)
	}
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			unlock()
		}
	}()
	done := make(chan error, 1)
	go func() { done <- Cancel(dir, true) }()
	select {
	case err := <-done:
		t.Fatalf("cancel returned before execution lock released: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	unlock()
	released = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not confirm release")
	}
	if !Cancelled(dir) {
		t.Fatal("cancel marker missing")
	}
	if err := CheckLaunch(dir); err == nil {
		t.Fatal("delayed old launch remained valid")
	}
}

func TestInterruptRejectsMalformedWorkerIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.pid")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Interrupt(dir); err == nil || !strings.Contains(err.Error(), "invalid process identity marker") {
		t.Fatalf("malformed identity was accepted: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "{" {
		t.Fatalf("unconfirmed historical identity was discarded: %q, %v", raw, err)
	}
}
