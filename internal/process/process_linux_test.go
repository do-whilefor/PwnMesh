//go:build linux

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func outputFile(t *testing.T, dir, name string) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func assertOutput(t *testing.T, f *os.File, want string) {
	t.Helper()
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Fatalf("%s = %q, want %q", filepath.Base(f.Name()), raw, want)
	}
}

func assertNoGroups(t *testing.T, dir string) {
	t.Helper()
	markers, err := filepath.Glob(filepath.Join(dir, "group-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 0 {
		t.Fatalf("process group markers remain: %v", markers)
	}
}

func TestRunStreamsSeparatesOutputAndPreservesExitStatus(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			dir := t.TempDir()
			stdout := outputFile(t, dir, "stdout")
			stderr := outputFile(t, dir, "stderr")
			err := RunStreams(context.Background(), dir, dir, stdout, stderr, "sh", "-c",
				fmt.Sprintf("printf '{\"ok\":true}\\n'; printf 'debug: diagnostic\\n' >&2; exit %d", code))
			if code == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != code {
					t.Fatalf("exit error = %v, want exit code %d", err, code)
				}
			}
			assertOutput(t, stdout, "{\"ok\":true}\n")
			assertOutput(t, stderr, "debug: diagnostic\n")
			assertNoGroups(t, dir)
		})
	}
}

func TestRunCombinesOutput(t *testing.T) {
	dir := t.TempDir()
	output := outputFile(t, dir, "output")
	if err := Run(context.Background(), dir, dir, output, "sh", "-c", "printf 'stdout\\n'; printf 'stderr\\n' >&2"); err != nil {
		t.Fatal(err)
	}
	assertOutput(t, output, "stdout\nstderr\n")
	assertNoGroups(t, dir)
}

func TestRunStreamsCancellationCleansUpChildren(t *testing.T) {
	dir := t.TempDir()
	stdout := outputFile(t, dir, "stdout")
	stderr := outputFile(t, dir, "stderr")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunStreams(ctx, dir, dir, stdout, stderr, "sh", "-c",
			"printf 'ready\\n'; printf 'debug\\n' >&2; sleep 30 & printf '%s' \"$!\" > child.pid; wait")
	}()
	var childPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join(dir, "child.pid"))
		if err == nil {
			childPID, err = strconv.Atoi(string(raw))
			if err == nil && childPID > 1 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 1 {
		t.Fatal("command did not start its child")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command did not stop after cancellation")
	}
	assertOutput(t, stdout, "ready\n")
	assertOutput(t, stderr, "debug\n")
	assertNoGroups(t, dir)
	// A terminated child may remain a zombie until the container's init reaps it.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", childPID))
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if i := strings.LastIndexByte(string(raw), ')'); i >= 0 {
			fields := strings.Fields(string(raw)[i+1:])
			if len(fields) > 0 && fields[0] == "Z" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child %d still running after cancellation", childPID)
}
