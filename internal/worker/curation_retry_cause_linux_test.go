//go:build linux

package worker

import (
	"context"
	"errors"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func curatorReceiptBridge(t *testing.T, dir string) *draftTestBridge {
	t.Helper()
	return &draftTestBridge{dir: dir, handle: func(request GraphRequest) (any, error) {
		if request.Op != "curate_receipt" {
			t.Fatalf("failed model unexpectedly published a mutation: %+v", request)
		}
		return board.StateActionResult{}, nil
	}}
}

func TestCuratorDeadlinePreservesOnlyTypedInfrastructureCause(t *testing.T) {
	for _, mode := range []string{"transport", "plain_deadline", "semantic_text", "provider_error"} {
		t.Run(mode, func(t *testing.T) {
			job, dir := curationJob(t), t.TempDir()
			job.Budget.Timeout = 1
			calls := 0
			opts := Options{RunDir: dir, Output: curatorReceiptBridge(t, dir), Provider: scenarioProvider(func(ctx context.Context, _ []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
				calls++
				<-ctx.Done()
				switch mode {
				case "transport":
					return agent.Message{}, &agent.ModelError{Kind: agent.ErrorTransport, Err: ctx.Err()}
				case "semantic_text":
					return agent.Message{}, errors.New("transport: quoted model text is not a typed transport failure")
				case "provider_error":
					return agent.Message{}, &agent.ModelError{Kind: agent.ErrorProvider, Err: ctx.Err()}
				default:
					return agent.Message{}, ctx.Err()
				}
			})}
			result, err := runTestWorker(context.Background(), job, opts)
			cause := ""
			if mode == "transport" {
				cause = "transport"
			}
			if err != nil || result.Status != "failed" || result.FailureKind != "budget_exhausted" || result.FailureCause != cause || result.Retryable {
				t.Fatalf("deadline lost or invented infrastructure provenance: %+v err=%v", result, err)
			}
			saved := outcomeSession(t, dir)
			if saved.Result == nil || saved.Result.FailureCause != cause {
				t.Fatal("terminal session did not retain failure provenance")
			}
			if replay, err := runTestWorker(context.Background(), job, opts); err != nil || replay.FailureCause != cause || replay.FailureKind != result.FailureKind || calls != 1 {
				t.Fatalf("terminal replay lost provenance or refreshed the original deadline: %+v err=%v calls=%d", replay, err, calls)
			}
		})
	}
}

func TestCuratorRecoveryExhaustionRetainsInfrastructureCause(t *testing.T) {
	job, dir := curationJob(t), t.TempDir()
	job.Budget.Timeout = 60
	calls := 0
	opts := Options{RunDir: dir, Output: curatorReceiptBridge(t, dir), Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		calls++
		return agent.Message{}, &agent.ModelError{Kind: agent.ErrorTransport, Err: errors.New("connection reset")}
	})}
	var result Result
	for attempt := 0; attempt <= maxRunRecoveries; attempt++ {
		var err error
		result, err = runTestWorker(context.Background(), job, opts)
		if err != nil {
			t.Fatal(err)
		}
		if attempt < maxRunRecoveries && (!result.Retryable || result.FailureKind != "transport") {
			t.Fatalf("same-run transport recovery stopped prematurely: %+v", result)
		}
	}
	if result.Retryable || result.FailureKind != "recovery_exhausted" || result.FailureCause != "transport" || calls != maxRunRecoveries+1 {
		t.Fatalf("recovery cap lost the actual cause or restarted indefinitely: %+v calls=%d", result, calls)
	}
}

func TestCuratorHumanCancellationDoesNotBecomeInfrastructureFailure(t *testing.T) {
	job, dir := curationJob(t), t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := runTestWorker(ctx, job, Options{RunDir: dir, Output: curatorReceiptBridge(t, dir), Provider: scenarioProvider(func(ctx context.Context, _ []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		cancel()
		<-ctx.Done()
		return agent.Message{}, &agent.ModelError{Kind: agent.ErrorTransport, Err: ctx.Err()}
	})})
	if !errors.Is(err, context.Canceled) || result.FailureCause != "" || result.Retryable {
		t.Fatalf("human cancellation acquired retry permission: %+v err=%v", result, err)
	}
}
