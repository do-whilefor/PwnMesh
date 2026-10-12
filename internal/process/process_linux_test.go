//go:build linux

package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
		// Reaping between open and read returns ESRCH instead of ENOENT.
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
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

func TestRunStreamsConfirmsDetachedChildrenStopped(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid unavailable")
	}
	dir := t.TempDir()
	output := outputFile(t, dir, "output")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := RunStreams(ctx, dir, dir, output, output, "sh", "-c",
		`setsid sh -c 'echo $$ > detached.pid; sleep 30 & echo $! > grandchild.pid; wait' & while [ ! -s grandchild.pid ]; do sleep 0.01; done`)
	for _, file := range []string{"detached.pid", "grandchild.pid"} {
		raw, readErr := os.ReadFile(filepath.Join(dir, file))
		if readErr != nil {
			t.Fatal(readErr)
		}
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
		if parseErr != nil || pid <= 1 {
			t.Fatalf("invalid %s: %q", file, raw)
		}
		t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
		if processRunning(pid) {
			t.Errorf("detached command process %d remains alive after RunStreams returned", pid)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	assertNoGroups(t, dir)
}

func TestKillGroupsRetainsUnconfirmedOwnership(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "group-2147483647")
	id := identity{PID: 2147483647, Start: "1", Token: "invalid"}
	write := func() {
		t.Helper()
		raw, err := json.Marshal(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if err := KillGroups(dir); err == nil {
		t.Fatal("invalid ownership token was accepted")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("unconfirmed process ownership was discarded: %v", err)
	}
	id.Token = strings.Repeat("a", 32)
	write()
	if err := KillGroups(dir); err != nil {
		t.Fatal(err)
	}
	assertNoGroups(t, dir)
}

func TestRunStreamsPreservesArgumentsAndRejectsStartFailures(t *testing.T) {
	dir := t.TempDir()
	output := outputFile(t, dir, "output")
	args := []string{"space here", "$(touch unexpected)", "; touch unexpected; #", "'quotes'"}
	command := append([]string{"-c", `printf '<%s>' "$@"`, "fixture"}, args...)
	if err := RunStreams(context.Background(), dir, dir, output, output, "sh", command...); err != nil {
		t.Fatal(err)
	}
	assertOutput(t, output, "<"+strings.Join(args, "><")+">")
	if _, err := os.Stat(filepath.Join(dir, "unexpected")); !os.IsNotExist(err) {
		t.Fatalf("command arguments were interpreted by the launch shell: %v", err)
	}
	if err := RunStreams(context.Background(), dir, dir, output, output, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing executable was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunStreams(ctx, dir, dir, output, output, "sh", "-c", "touch unexpected"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled startup = %v", err)
	}
	assertNoGroups(t, dir)
}
