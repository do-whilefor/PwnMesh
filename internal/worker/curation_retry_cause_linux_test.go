//go:build linux

package worker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/provider"
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

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func TestCuratorHTTPBackoffPreservesDeadlineCauseWithoutRetryingCancellation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		cause     string
		cancelled bool
	}{
		{"unavailable deadline", http.StatusServiceUnavailable, "unavailable", false},
		{"rate limit deadline", http.StatusTooManyRequests, "rate_limit", false},
		{"unavailable cancellation", http.StatusServiceUnavailable, "", true},
		{"rate limit cancellation", http.StatusTooManyRequests, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				time.Sleep(150 * time.Millisecond)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"temporary provider failure"}}`))
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := server.Client()
			if tc.cancelled {
				transport := client.Transport
				client.Transport = recoveryRoundTrip(func(req *http.Request) (*http.Response, error) {
					response, err := transport.RoundTrip(req)
					if err == nil {
						// Cancel only after the error response was consumed, at the
						// retry boundary rather than during the HTTP request.
						response.Body = cancelOnCloseBody{response.Body, cancel}
					}
					return response, err
				})
			}
			job, dir := curationJob(t), t.TempDir()
			job.Budget.Timeout = 1
			if tc.cancelled {
				job.Budget.Timeout = 60
			}
			p := &provider.Anthropic{BaseURL: server.URL, Token: "fixture", Timeout: 5 * time.Second, Client: client}
			opts := Options{RunDir: dir, Output: curatorReceiptBridge(t, dir), Provider: p}
			result, err := Run(ctx, job, opts)
			if calls.Load() != 1 {
				t.Fatalf("interrupted backoff sent another request: calls=%d", calls.Load())
			}
			if tc.cancelled {
				if !errors.Is(err, context.Canceled) || result.FailureCause != "" || result.Retryable || outcomeSession(t, dir).Result != nil {
					t.Fatalf("human cancellation acquired a retryable terminal result: %+v err=%v", result, err)
				}
				return
			}
			if err != nil || result.Status != "failed" || result.FailureKind != "budget_exhausted" || result.FailureCause != tc.cause || result.Retryable {
				t.Fatalf("HTTP %d backoff lost infrastructure provenance: %+v err=%v", tc.status, result, err)
			}
			if saved := outcomeSession(t, dir); saved.Result == nil || saved.Result.FailureCause != tc.cause {
				t.Fatalf("terminal session lost infrastructure provenance: %+v", saved.Result)
			}
			if replay, err := Run(ctx, job, opts); err != nil || replay.FailureCause != tc.cause || replay.FailureKind != result.FailureKind || calls.Load() != 1 {
				t.Fatalf("terminal replay lost provenance or refreshed the deadline: %+v err=%v calls=%d", replay, err, calls.Load())
			}
		})
	}
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
