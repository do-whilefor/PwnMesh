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

	"pwnmesh/internal/agent"
	"pwnmesh/internal/workergraph"
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

// Build the old on-disk protocol independently of the migration adapter. The
// original scheduler binds the definition, inputs, attempts and dependencies.
func writeLegacyAcceptanceFixture(t *testing.T, job Job, dir string, out acceptanceOutput, phase string) {
	t.Helper()
	identity, err := identityFor(job, dir)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(identity)
	noop := func(context.Context, workergraph.Input) (workergraph.Output, error) { return workergraph.Output{}, nil }
	verify := func(context.Context, workergraph.Input, workergraph.Output) error { return nil }
	definition := workergraph.Definition{Version: "execute-v1", Nodes: []workergraph.Node{
		{ID: "prepare", Kind: "function", Run: noop, Verify: verify},
		{ID: "agent", Kind: "agent", DependsOn: []workergraph.Dependency{{ID: "prepare"}}, Verify: verify, Run: func(context.Context, workergraph.Input) (workergraph.Output, error) {
			if phase == "agent_interrupted" {
				return workergraph.Output{}, workergraph.ErrInterrupted
			}
			if phase == "failed" {
				return workergraph.Output{}, errors.New("original deterministic failure")
			}
			converted := workergraph.Output{Value: out.Value}
			for _, artifact := range out.Artifacts {
				converted.Artifacts = append(converted.Artifacts, workergraph.Artifact{Path: artifact.Path, SHA256: artifact.SHA256})
			}
			return converted, nil
		}},
		{ID: "accept", Kind: "function", DependsOn: []workergraph.Dependency{{ID: "agent"}}, Verify: verify, Run: func(_ context.Context, in workergraph.Input) (workergraph.Output, error) {
			if phase == "accept_interrupted" {
				return workergraph.Output{}, workergraph.ErrInterrupted
			}
			return in.Dependencies[0].Output, nil
		}},
	}}
	_, err = workergraph.Run(context.Background(), definition, workergraph.Options{RunID: job.RunID, Input: input, Dir: filepath.Join(dir, "graph"), Parallelism: 1})
	if (phase == "accepted") != (err == nil) {
		t.Fatalf("cannot build legacy %s checkpoint: %v", phase, err)
	}
}

func TestAcceptanceMigratesLegacyCheckpointsWithoutRepeatingModelWork(t *testing.T) {
	for _, phase := range []string{"accepted", "accept_interrupted", "agent_interrupted", "failed", "unbound_node_hashes", "changed_input", "changed_session"} {
		t.Run(phase, func(t *testing.T) {
			job, dir := graphWrapperJob(t), t.TempDir()
			var output bytes.Buffer
			calls := 0
			options := Options{RunDir: dir, Output: &output, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
				calls++
				return agent.Text("assistant", completedOutput("explore")), nil
			})}
			first, err := runTestWorker(context.Background(), job, options)
			if err != nil || first.Status != "success" {
				t.Fatalf("initial session: %+v %v", first, err)
			}
			receipt := readAcceptanceReceipt(t, dir)
			fixturePhase := phase
			if strings.HasPrefix(phase, "changed_") || phase == "unbound_node_hashes" {
				fixturePhase = "accepted"
			}
			writeLegacyAcceptanceFixture(t, job, dir, receipt.Output, fixturePhase)
			if err := os.Remove(filepath.Join(dir, "graph", "acceptance.json")); err != nil {
				t.Fatal(err)
			}
			switch phase {
			case "changed_input":
				job.RunID += "-different"
			case "changed_session":
				if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(`{"changed":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "unbound_node_hashes":
				path := filepath.Join(dir, "graph", "graph.json")
				raw, err := os.ReadFile(path)
				var saved workergraph.Checkpoint
				if err != nil || json.Unmarshal(raw, &saved) != nil {
					t.Fatal("cannot read legacy checkpoint")
				}
				for i := range saved.Nodes {
					saved.Nodes[i].DefinitionSHA256 = ""
				}
				raw, _ = json.Marshal(saved)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			output.Reset()
			result, err := runTestWorker(context.Background(), job, options)
			wantSuccess := phase != "failed" && !strings.HasPrefix(phase, "changed_")
			if err != nil || (result.Status == "success") != wantSuccess || calls != 1 || len(graphOutputResults(t, output.Bytes())) != 1 {
				t.Fatalf("legacy %s migration: %+v %v calls=%d output=%s", phase, result, err, calls, &output)
			}
			if wantSuccess {
				migrated := readAcceptanceReceipt(t, dir)
				if migrated.Status != "accepted" || !bytes.Equal(migrated.Output.Value, receipt.Output.Value) {
					t.Fatalf("migration did not preserve the accepted result: %+v", migrated)
				}
				// Once migrated, even a damaged obsolete graph cannot redirect
				// execution back into the previous scheduler.
				if err := os.WriteFile(filepath.Join(dir, "graph", "graph.json"), []byte("obsolete"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			output.Reset()
			again, err := runTestWorker(context.Background(), job, options)
			if err != nil || (again.Status == "success") != wantSuccess || calls != 1 || len(graphOutputResults(t, output.Bytes())) != 1 {
				t.Fatalf("repeated migration changed outcome: %+v %v calls=%d", again, err, calls)
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
				return graphSessionFixture(ctx, job, opts, Result{Type: "result", Status: "success", Text: completedOutput("explore")})
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

func TestAcceptanceInterruptedMigrationRetainsTheLegacyBoundary(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	writeLegacyAcceptanceFixture(t, job, dir, acceptanceOutput{}, "agent_interrupted")
	var output bytes.Buffer
	calls := 0
	sessionRun := func(ctx context.Context, job Job, opts Options) (Result, error) {
		calls++
		if calls == 1 {
			return Result{}, io.ErrUnexpectedEOF
		}
		return graphSessionFixture(ctx, job, opts, Result{Type: "result", Status: "success", Text: completedOutput("explore")})
	}
	opts := Options{RunDir: dir, Output: &output}
	if _, err := runWorkerAcceptance(context.Background(), job, opts, sessionRun); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("migration lost uncertain callback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "graph", "acceptance.json")); !os.IsNotExist(err) {
		t.Fatal("interrupted migration replaced the original recovery boundary")
	}
	if len(graphOutputResults(t, output.Bytes())) != 0 {
		t.Fatal("interrupted migration published a terminal result")
	}
	result, err := runWorkerAcceptance(context.Background(), job, opts, sessionRun)
	if err != nil || result.Status != "success" || calls != 2 || readAcceptanceReceipt(t, dir).Status != "accepted" || len(graphOutputResults(t, output.Bytes())) != 1 {
		t.Fatalf("legacy recovery did not migrate after acceptance: %+v %v calls=%d", result, err, calls)
	}
}

func TestAcceptanceDeterministicFailureNeverRestartsSession(t *testing.T) {
	job, dir := graphWrapperJob(t), t.TempDir()
	var output bytes.Buffer
	calls := 0
	sessionRun := func(context.Context, Job, Options) (Result, error) {
		calls++
		// A returned candidate without its durable session cannot be accepted.
		return Result{Type: "result", Status: "success", Text: completedOutput("explore")}, nil
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
				return graphSessionFixture(ctx, job, opts, Result{Type: "result", Status: "success", Text: completedOutput("explore")})
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
