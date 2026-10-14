//go:build linux

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readAcceptanceReceipt(t *testing.T, dir string) acceptanceReceipt {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "graph", "acceptance.json"))
	var receipt acceptanceReceipt
	if err != nil || json.Unmarshal(raw, &receipt) != nil {
		t.Fatalf("missing acceptance receipt: %s %v", raw, err)
	}
	return receipt
}

func TestAcceptanceRejectsRetiredCheckpointBeforeEffects(t *testing.T) {
	for _, kind := range []string{"file", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "graph.json")
			switch kind {
			case "file":
				if err := os.WriteFile(path, []byte(`{"version":"execute-v1","status":"accepted"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("missing-checkpoint", path); err != nil {
					t.Fatal(err)
				}
			}
			called, verified := 0, 0
			call := func(context.Context) (acceptanceOutput, error) { called++; return acceptanceOutput{}, nil }
			verify := func(context.Context, acceptanceOutput) error { verified++; return nil }
			for attempt := 0; attempt < 2; attempt++ {
				_, err := acceptWorkerSession(context.Background(), "retired-run", json.RawMessage(`{}`), dir, call, verify)
				if err == nil || !strings.Contains(err.Error(), "unsupported execute-v1 acceptance checkpoint") || called != 0 || verified != 0 {
					t.Fatalf("retired checkpoint reached a callback: %v calls=%d verifies=%d", err, called, verified)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "graph.json" {
				t.Fatalf("retired checkpoint directory was changed: %v %v", entries, err)
			}
		})
	}
}

func TestWorkerAcceptanceRejectsRetiredCheckpointBeforeWrites(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	graphDir := filepath.Join(dir, "graph")
	if err := os.Mkdir(graphDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(graphDir, "graph.json")
	original := []byte(`{"version":"execute-v1","status":"running"}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	calls := 0
	sessionRun := func(context.Context, Job, Options) (Result, error) { calls++; return Result{}, nil }
	for attempt := 0; attempt < 2; attempt++ {
		_, err := runWorkerAcceptance(context.Background(), job, Options{RunDir: dir, Output: &output}, sessionRun)
		if err == nil || !strings.Contains(err.Error(), "unsupported execute-v1 acceptance checkpoint") || calls != 0 || output.Len() != 0 {
			t.Fatalf("retired checkpoint started or published a run: %v calls=%d output=%s", err, calls, &output)
		}
	}
	for _, directory := range []string{dir, graphDir} {
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 1 {
			t.Fatalf("retired run wrote new files: %v %v", entries, err)
		}
	}
	if retained, err := os.ReadFile(path); err != nil || !bytes.Equal(retained, original) {
		t.Fatalf("retired checkpoint was rewritten: %s %v", retained, err)
	}
}

func TestAcceptanceCurrentReceiptTakesPrecedenceOverRetiredGraph(t *testing.T) {
	for _, status := range []string{"accepted", "running", "failed"} {
		t.Run(status, func(t *testing.T) {
			job, dir := graphWrapperJob(t), t.TempDir()
			var output bytes.Buffer
			calls := 0
			sessionRun := func(ctx context.Context, job Job, opts Options) (Result, error) {
				calls++
				return graphSessionFixture(ctx, job, opts, Result{Type: "result", Status: "success", Text: completedOutput})
			}
			opts := Options{RunDir: dir, Output: &output}
			if result, err := runWorkerAcceptance(context.Background(), job, opts, sessionRun); err != nil || result.Status != "success" {
				t.Fatalf("initial acceptance: %+v %v", result, err)
			}
			receipt := readAcceptanceReceipt(t, dir)
			receipt.Status = status
			if status != "accepted" {
				receipt.Output = acceptanceOutput{}
			}
			if status == "failed" {
				receipt.Error = "original deterministic failure"
			}
			if err := saveAcceptance(filepath.Join(dir, "graph"), receipt); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "graph", "graph.json")
			original := []byte("obsolete execute-v1 checkpoint")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				output.Reset()
				result, err := runWorkerAcceptance(context.Background(), job, opts, sessionRun)
				wantCalls := 1
				if status == "running" {
					wantCalls = 2
				}
				if err != nil || (result.Status == "success") != (status != "failed") || calls != wantCalls || len(graphOutputResults(t, output.Bytes())) != 1 {
					t.Fatalf("current %s receipt lost recovery: %+v %v calls=%d", status, result, err, calls)
				}
			}
			current := readAcceptanceReceipt(t, dir)
			if current.SchemaVersion != 1 || current.InputSHA256 != receipt.InputSHA256 || current.RunID != receipt.RunID {
				t.Fatalf("current receipt identity changed: %+v", current)
			}
			if status == "accepted" && !bytes.Equal(current.Output.Value, receipt.Output.Value) {
				t.Fatal("accepted result bytes changed")
			}
			if retained, err := os.ReadFile(path); err != nil || !bytes.Equal(retained, original) {
				t.Fatalf("obsolete graph was read or rewritten: %s %v", retained, err)
			}
		})
	}
}

func TestAcceptanceSessionErrorsAndPanicsRetainRecoveryBoundary(t *testing.T) {
	for _, mode := range []string{"error", "panic"} {
		t.Run(mode, func(t *testing.T) {
			job, dir := graphWrapperJob(t), t.TempDir()
			var output bytes.Buffer
			calls := 0
			sessionRun := func(ctx context.Context, job Job, opts Options) (Result, error) {
				calls++
				if calls == 1 {
					if mode == "panic" {
						panic("interrupted callback")
					}
					return Result{}, io.ErrUnexpectedEOF
				}
				return graphSessionFixture(ctx, job, opts, Result{Type: "result", Status: "success", Text: completedOutput})
			}
			opts := Options{RunDir: dir, Output: &output}
			if _, err := runWorkerAcceptance(context.Background(), job, opts, sessionRun); !errors.Is(err, ErrInterrupted) || len(graphOutputResults(t, output.Bytes())) != 0 {
				t.Fatalf("%s lost interruption: %v output=%s", mode, err, &output)
			}
			if receipt := readAcceptanceReceipt(t, dir); receipt.Status != "running" || len(receipt.Output.Value) != 0 {
				t.Fatalf("uncertain session was accepted: %+v", receipt)
			}
			result, err := runWorkerAcceptance(context.Background(), job, opts, sessionRun)
			if err != nil || result.Status != "success" || calls != 2 || len(graphOutputResults(t, output.Bytes())) != 1 {
				t.Fatalf("same session recovery failed: %+v %v calls=%d", result, err, calls)
			}
		})
	}
}

func TestAcceptanceDeterministicFailureNeverRestartsSession(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	var output bytes.Buffer
	calls := 0
	sessionRun := func(context.Context, Job, Options) (Result, error) {
		calls++
		// A returned candidate without its durable session cannot be accepted.
		return Result{Type: "result", Status: "success", Text: completedOutput}, nil
	}
	options := Options{RunDir: dir, Output: &output}
	for attempt := 0; attempt < 2; attempt++ {
		output.Reset()
		result, err := runWorkerAcceptance(context.Background(), job, options, sessionRun)
		if err != nil || result.Status != "failed" || result.FailureKind != "graph_checkpoint" || calls != 1 || len(graphOutputResults(t, output.Bytes())) != 1 {
			t.Fatalf("failure authorized another session: %+v %v calls=%d", result, err, calls)
		}
	}
	if receipt := readAcceptanceReceipt(t, dir); receipt.Status != "failed" || receipt.Error == "" {
		t.Fatalf("deterministic failure not retained: %+v", receipt)
	}
}

func TestAcceptanceRejectsChangedSessionOrMalformedReceiptBeforeRecovery(t *testing.T) {
	for _, mode := range []string{"session", "identity", "status", "missing_session_digest", "retryable_result"} {
		t.Run(mode, func(t *testing.T) {
			job, dir := graphWrapperJob(t), t.TempDir()
			var output bytes.Buffer
			calls := 0
			sessionRun := func(ctx context.Context, job Job, opts Options) (Result, error) {
				calls++
				return graphSessionFixture(ctx, job, opts, Result{Type: "result", Status: "success", Text: completedOutput})
			}
			opts := Options{RunDir: dir, Output: &output}
			if _, err := runWorkerAcceptance(context.Background(), job, opts, sessionRun); err != nil {
				t.Fatal(err)
			}
			receipt := readAcceptanceReceipt(t, dir)
			switch mode {
			case "session":
				if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(`{"changed":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "identity":
				receipt.InputSHA256 = strings.Repeat("a", 64)
			case "status":
				receipt.Status = "unknown"
			case "missing_session_digest":
				receipt.Output.Artifacts = nil
			case "retryable_result":
				receipt.Output.Value = json.RawMessage(`{"type":"result","status":"failed","retryable":true}`)
			}
			if err := saveAcceptance(filepath.Join(dir, "graph"), receipt); err != nil {
				t.Fatal(err)
			}
			output.Reset()
			result, err := runWorkerAcceptance(context.Background(), job, opts, sessionRun)
			if err != nil || result.Status != "failed" || calls != 1 || len(graphOutputResults(t, output.Bytes())) != 1 {
				t.Fatalf("invalid %s reused: %+v %v calls=%d", mode, result, err, calls)
			}
		})
	}
}
