//go:build linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestToolOutputCaptureHasHardLimit(t *testing.T) {
	const limit = 64 << 20
	for _, name := range []string{"bash", "read"} {
		t.Run(name, func(t *testing.T) {
			s := fidelitySet(t)
			s.OutputBytes = 128
			args := map[string]any{"command": fmt.Sprintf("printf diagnostic >&2; head -c %d /dev/zero; sleep 30", limit+1)}
			if name == "read" {
				path := filepath.Join(s.Dir, "long-line")
				putFidelityFile(t, path, nil)
				if err := os.Truncate(path, limit+1); err != nil {
					t.Fatal(err)
				}
				args = map[string]any{"path": path, "limit": 1}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := callFidelityTool(t, s, ctx, name, args)
			if err == nil || !strings.Contains(err.Error(), "output exceeds") {
				t.Fatalf("oversized output was not rejected: %v", err)
			}
			if ctx.Err() != nil {
				t.Fatal("output limit did not stop execution before the parent deadline")
			}
			if !strings.Contains(output, "Partial output:") || strings.Contains(output, "Full output:") {
				t.Fatalf("clipped output was advertised as complete: %q", output)
			}
			files, err := filepath.Glob(filepath.Join(s.RunDir, "output-*.txt"))
			if err != nil || len(files) != 1 {
				t.Fatalf("missing retained diagnostic: %v %v", files, err)
			}
			info, err := os.Stat(files[0])
			if err != nil || info.Size() != limit {
				t.Fatalf("capture exceeded or lost its bounded prefix: %v %v", info, err)
			}
			markers, err := filepath.Glob(filepath.Join(s.RunDir, "group-*"))
			if err != nil || len(markers) != 0 {
				t.Fatalf("output cancellation left a process group marker: %v %v", markers, err)
			}
		})
	}
}

func TestBashOutputLimitDoesNotLimitArtifacts(t *testing.T) {
	s := fidelitySet(t)
	const size = 128 << 20
	output, err := callFidelityTool(t, s, context.Background(), "bash", map[string]any{
		"command": fmt.Sprintf("truncate -s %d artifact; printf complete", size),
	})
	if err != nil || output != "complete" {
		t.Fatalf("ordinary command output failed: %q %v", output, err)
	}
	info, err := os.Stat(filepath.Join(s.Dir, "artifact"))
	if err != nil || info.Size() != size {
		t.Fatalf("output capture changed the artifact allowance: %v %v", info, err)
	}
}

func TestBashCaptureDeadlineClosesInheritedOutputPipe(t *testing.T) {
	s := fidelitySet(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	_, err := callFidelityTool(t, s, ctx, "bash", map[string]any{
		// The finite holder deliberately has neither the ownership token nor
		// the shell's process group, but retains stdout after its parent exits.
		"command": `env -i PATH="$PATH" setsid sh -c 'printf %s "$$" > holder.pid; exec sleep 5' & while [ ! -s holder.pid ]; do sleep 0.01; done`,
	})
	elapsed := time.Since(started)
	raw, readErr := os.ReadFile(filepath.Join(s.Dir, "holder.pid"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(string(raw))
	if parseErr != nil || pid <= 1 {
		t.Fatalf("invalid owned pipe holder: %q %v", raw, parseErr)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	if !errors.Is(err, context.DeadlineExceeded) || elapsed >= 3*time.Second {
		t.Fatalf("capture outlived its deadline: elapsed=%s err=%v", elapsed, err)
	}
}
