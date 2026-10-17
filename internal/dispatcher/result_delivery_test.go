package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/protocol"
)

func TestWorkerFailureRetainsReportedUsage(t *testing.T) {
	for _, mode := range []string{"infrastructure", "resumed_infrastructure", "cancelled", "dependency_invalidated"} {
		t.Run(mode, func(t *testing.T) {
			s, runner, store, run := resultDeliveryFixture(t)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			usage := agent.Usage{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 19, CacheWriteTokens: 37}
			metrics := &protocol.DecisionMetrics{
				Version: 1, UsageScope: "run_and_children", ModelCalls: 1, UsageCalls: 1,
				Usage: usage, UsageStatus: "reported_only", CostStatus: "unknown",
			}
			if mode == "resumed_infrastructure" {
				var err error
				run.Execution.Result, err = json.Marshal(protocol.Result{Status: "failed", Metrics: metrics})
				if err != nil {
					t.Fatal(err)
				}
				metrics = nil // The resumed transport fails before another observation.
			}
			runner.run = func(context.Context, protocol.Job) (protocol.Result, error) {
				if mode == "resumed_infrastructure" {
					var stored protocol.Result
					if err := json.Unmarshal(curationExecution(t, store, run).Result, &stored); err != nil {
						t.Fatal(err)
					}
					if stored.Metrics == nil || stored.Metrics.Usage != usage || stored.Text != "" {
						t.Fatalf("running update discarded saved usage or retained acceptance: %+v", stored)
					}
				}
				if mode == "cancelled" {
					cancel(context.Canceled)
				} else if mode == "dependency_invalidated" {
					cancel(&protocol.RPCError{Status: 409, Code: board.CodeDependencyInvalidated, Detail: "source invalidated"})
				}
				// Same-run responses carry cumulative metrics; retries must not add them.
				return protocol.Result{Status: "success", Text: "unaccepted result", Metrics: metrics}, errors.New("worker response failed")
			}
			outcome, err := s.runTask(ctx, run)
			wantStatus, wantFailure, wantCalls := "failed", "transient_infrastructure", int32(3)
			if mode == "cancelled" || mode == "dependency_invalidated" {
				wantStatus, wantFailure, wantCalls = "cancelled", "hard_cancelled", 1
				if mode == "dependency_invalidated" {
					wantFailure = mode
				}
			}
			if outcome != wantStatus || err == nil || runner.calls.Load() != wantCalls {
				t.Fatalf("unexpected failure behavior: outcome=%s err=%v calls=%d", outcome, err, runner.calls.Load())
			}
			execution := curationExecution(t, store, run)
			var result protocol.Result
			if err := json.Unmarshal(execution.Result, &result); err != nil {
				t.Fatal(err)
			}
			if execution.Status != wantStatus || result.FailureKind != wantFailure || result.Retryable || result.Text != "" {
				t.Fatalf("failure retained acceptance or changed classification: execution=%s result=%+v", execution.Status, result)
			}
			if result.Metrics == nil || result.Metrics.Usage != usage || result.Metrics.ModelCalls != 1 || result.Metrics.UsageScope != "run_and_children" {
				t.Fatalf("failure lost or duplicated observed usage: %+v", result.Metrics)
			}
		})
	}
}

func TestRecoveryTerminalRetainsSavedUsage(t *testing.T) {
	s, _, store, run := resultDeliveryFixture(t)
	metrics := &protocol.DecisionMetrics{Version: 1, Usage: agent.Usage{CacheWriteTokens: 37}, UsageStatus: "reported_only", CostStatus: "unknown"}
	var err error
	run.Execution.Result, err = json.Marshal(protocol.Result{Status: "success", Text: "unaccepted", Metrics: metrics})
	if err != nil {
		t.Fatal(err)
	}
	// Recovery can reject the environment or resume grant before starting a run.
	s.terminal(run, "failed", protocol.Result{Status: "failed", FailureKind: "configuration", Error: "registered execution environment changed"})
	var stored protocol.Result
	if err := json.Unmarshal(curationExecution(t, store, run).Result, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Metrics == nil || stored.Metrics.Usage != metrics.Usage || stored.FailureKind != "configuration" || stored.Text != "" {
		t.Fatalf("recovery failure discarded saved usage or retained acceptance: %+v", stored)
	}
}

func TestParentUsageSnapshotRetainsKnownChildrenAcrossRetries(t *testing.T) {
	s, runner, store, run := resultDeliveryFixture(t)
	children := &protocol.ChildUsage{Sessions: 1, ModelCalls: 2, UsageCalls: 2, Usage: agent.Usage{CacheWriteTokens: 37}}
	prior := &protocol.DecisionMetrics{Version: 1, UsageScope: "run_and_children", ModelCalls: 3, UsageCalls: 3, Usage: agent.Usage{CacheWriteTokens: 48}, ChildUsage: children}
	var err error
	run.Execution.Result, err = json.Marshal(protocol.Result{Metrics: prior})
	if err != nil {
		t.Fatal(err)
	}
	runner.run = func(context.Context, protocol.Job) (protocol.Result, error) {
		return protocol.Result{Metrics: &protocol.DecisionMetrics{Version: 1, UsageScope: "run", ModelCalls: 2, CompletedCalls: 2, UsageCalls: 2, UsageStatus: "partial", Usage: agent.Usage{CacheWriteTokens: 23}}}, errors.New("interrupted before final result")
	}
	if outcome, err := s.runTask(context.Background(), run); outcome != "failed" || err == nil {
		t.Fatalf("unexpected result: %s %v", outcome, err)
	}
	var saved protocol.Result
	if err := json.Unmarshal(curationExecution(t, store, run).Result, &saved); err != nil {
		t.Fatal(err)
	}
	metrics := saved.Metrics
	if metrics == nil || metrics.Usage.CacheWriteTokens != 60 || metrics.ModelCalls != 4 || metrics.CompletedCalls != 4 || metrics.UsageCalls != 4 || metrics.UsageScope != "run_and_children" || metrics.UsageStatus != "partial" || metrics.ChildUsage == nil || !metrics.ChildUsage.Incomplete || metrics.ChildUsage.Usage.CacheWriteTokens != 37 {
		t.Fatalf("parent-only snapshots lost or duplicated known children: %+v", metrics)
	}
}

func TestFinalUsageKeepsKnownChildrenOnlyWhenAggregationIsPartial(t *testing.T) {
	for _, incomplete := range []bool{true, false} {
		s, runner, store, run := resultDeliveryFixture(t)
		prior := &protocol.DecisionMetrics{Version: 1, UsageScope: "run_and_children", ModelCalls: 3, CompletedCalls: 3, UsageCalls: 3, Usage: agent.Usage{CacheWriteTokens: 48}, ChildUsage: &protocol.ChildUsage{Sessions: 1, ModelCalls: 2, CompletedCalls: 2, SummaryCalls: 1, FailedCalls: 1, UsageCalls: 2, ModelDurationMS: 10, TotalInputBytes: 150, Usage: agent.Usage{CacheWriteTokens: 37}}}
		var err error
		run.Execution.Result, err = json.Marshal(protocol.Result{Metrics: prior})
		if err != nil {
			t.Fatal(err)
		}
		runner.run = func(context.Context, protocol.Job) (protocol.Result, error) {
			return protocol.Result{Status: "failed", FailureKind: "fixture", Metrics: &protocol.DecisionMetrics{Version: 1, UsageScope: "run_and_children", ModelCalls: 2, CompletedCalls: 2, UsageCalls: 2, Usage: agent.Usage{CacheWriteTokens: 23}, ChildUsage: &protocol.ChildUsage{Incomplete: incomplete}}}, nil
		}
		if outcome, _ := s.runTask(context.Background(), run); outcome != "failed" {
			t.Fatal(outcome)
		}
		var saved protocol.Result
		if err := json.Unmarshal(curationExecution(t, store, run).Result, &saved); err != nil {
			t.Fatal(err)
		}
		m := saved.Metrics
		if incomplete {
			if m.Usage.CacheWriteTokens != 60 || m.CompletedCalls != 4 || m.ModelCalls != 4 || m.SummaryCalls != 1 || m.FailedCalls != 1 || m.ModelDurationMS != 10 || m.TotalInputBytes != 150 || m.UsageStatus != "partial" {
				t.Fatalf("incomplete final erased prior child observations: %+v", m)
			}
		} else if m.Usage.CacheWriteTokens != 23 || m.ModelCalls != 2 || m.CompletedCalls != 2 || m.ChildUsage.ModelCalls != 0 {
			t.Fatalf("complete final did not replace retained subtotal: %+v", m)
		}
	}
}

func TestDispatcherShutdownPersistsUsageWithoutFinalResult(t *testing.T) {
	s, runner, store, run := resultDeliveryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run.Root = ctx
	metrics := &protocol.DecisionMetrics{Version: 1, UsageScope: "run", ModelCalls: 1, UsageCalls: 1, UsageStatus: "partial", Usage: agent.Usage{CacheWriteTokens: 37}}
	runner.run = func(context.Context, protocol.Job) (protocol.Result, error) {
		cancel()
		return protocol.Result{Metrics: metrics}, context.Canceled
	}
	if outcome, err := s.runTask(ctx, run); outcome != "interrupted" || !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown changed recovery behavior: %s %v", outcome, err)
	}
	execution := curationExecution(t, store, run)
	var saved protocol.Result
	if err := json.Unmarshal(execution.Result, &saved); err != nil {
		t.Fatal(err)
	}
	if execution.Status != "running" || saved.Metrics == nil || saved.Metrics.Usage != metrics.Usage || saved.Status != "" || saved.Text != "" {
		t.Fatalf("shutdown discarded usage or persisted acceptance: status=%s result=%+v", execution.Status, saved)
	}
}

func resultDeliveryFixture(t *testing.T) (*Scheduler, *staleCuratorRunner, *board.Store, *task) {
	t.Helper()
	s, runner, store, graph := staleCuratorFixture(t)
	s.Config.Runtime.Interval = 30
	steps := authorizeFixtureSteps(t, s, graph.Project.ID, map[string]any{"from": []string{"origin"}, "description": "Observe result delivery"})
	run := prepareCurationTestTask(t, s, graph, "explore", "result-delivery", &steps[0])
	return s, runner, store, run
}

func TestResultPublicationRejectsInvalidReceiptWithoutRecovery(t *testing.T) {
	s, runner, store, run := resultDeliveryFixture(t)
	runner.run = func(ctx context.Context, job protocol.Job) (protocol.Result, error) {
		result, err := runner.observe(ctx, job)
		// The real Server rejects a repair receipt on an ordinary Step at the
		// result_pending boundary, before its business apply is attempted.
		result.RepairCheck = &protocol.RepairCheck{}
		return result, err
	}
	outcome, err := s.runTask(context.Background(), run)
	if outcome != "failed" || err == nil || !strings.Contains(err.Error(), "ordinary task cannot submit a repair receipt") {
		t.Fatalf("invalid result did not fail at publication: outcome=%s err=%v", outcome, err)
	}
	execution := curationExecution(t, store, run)
	var result protocol.Result
	if err := json.Unmarshal(execution.Result, &result); err != nil {
		t.Fatal(err)
	}
	if execution.Status != "failed" || result.FailureKind != "invalid_output" || result.Retryable || execution.Resumes != 0 {
		t.Fatalf("invalid output retained a recoverable attempt: status=%s resumes=%d result=%+v", execution.Status, execution.Resumes, result)
	}
	if err := s.loadExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverExecutions(context.Background(), map[string]string{run.Job.Graph.Project.ID: "active"}); err != nil {
		t.Fatal(err)
	}
	s.wg.Wait()
	if runner.calls.Load() != 1 {
		t.Fatal("a permanently rejected result restarted its Worker")
	}
}

func TestResultDeliveryTemporaryFailuresRecoverTheSameRun(t *testing.T) {
	for _, phase := range []string{"result_pending", "apply"} {
		for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
			t.Run(phase+"/"+http.StatusText(status), func(t *testing.T) {
				s, runner, store, run := resultDeliveryFixture(t)
				var saved protocol.Result
				runner.run = func(ctx context.Context, job protocol.Job) (protocol.Result, error) {
					if digest(job) != digest(run.Job) {
						t.Error("delivery recovery changed the registered input")
					}
					if saved.Status == "" {
						var err error
						saved, err = runner.observe(ctx, job)
						return saved, err
					}
					return saved, nil // A real Worker retains its completed result.
				}
				var failed atomic.Bool
				s.Client.HTTP = &http.Client{Transport: executionQueryTransport(func(request *http.Request) (*http.Response, error) {
					target := request.URL.Path == executionPath(run)+"/apply" && phase == "apply"
					if phase == "result_pending" && request.URL.Path == executionPath(run)+"/status" {
						body, err := request.GetBody()
						if err != nil {
							return nil, err
						}
						var update struct{ Status string }
						err = json.NewDecoder(body).Decode(&update)
						_ = body.Close()
						if err != nil {
							return nil, err
						}
						target = update.Status == "result_pending"
					}
					if target && failed.CompareAndSwap(false, true) {
						return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"detail":"temporary delivery failure"}`)), Request: request}, nil
					}
					return http.DefaultTransport.RoundTrip(request)
				})}
				outcome, err := s.runTask(context.Background(), run)
				if outcome != "interrupted" || err == nil || !failed.Load() {
					t.Fatalf("temporary delivery failure became terminal: outcome=%s err=%v", outcome, err)
				}
				execution := curationExecution(t, store, run)
				wantStatus, wantCalls := "running", int32(2)
				wantResumes := 1
				if phase == "apply" {
					wantStatus, wantCalls = "result_pending", 1
					wantResumes = 0
				}
				if execution.Status != wantStatus || execution.Resumes != 0 {
					t.Fatalf("delivery lost its recovery boundary: status=%s resumes=%d", execution.Status, execution.Resumes)
				}
				if err := s.loadExecutions(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := s.recoverExecutions(context.Background(), map[string]string{run.Job.Graph.Project.ID: "active"}); err != nil {
					t.Fatal(err)
				}
				s.wg.Wait()
				execution = curationExecution(t, store, run)
				if execution.Status != "succeeded" || execution.Resumes != wantResumes || runner.calls.Load() != wantCalls {
					t.Fatalf("same-run delivery did not settle: status=%s resumes=%d worker_calls=%d", execution.Status, execution.Resumes, runner.calls.Load())
				}
			})
		}
	}
}
