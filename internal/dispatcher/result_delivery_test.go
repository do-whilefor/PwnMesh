package dispatcher

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

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
	runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
		result, err := runner.observe(ctx, job)
		// The real Server rejects a repair receipt on an ordinary Step at the
		// result_pending boundary, before its business apply is attempted.
		result.RepairCheck = &worker.RepairCheck{}
		return result, err
	}
	outcome, err := s.runTask(context.Background(), run)
	if outcome != "failed" || err == nil || !strings.Contains(err.Error(), "ordinary task cannot submit a repair receipt") {
		t.Fatalf("invalid result did not fail at publication: outcome=%s err=%v", outcome, err)
	}
	execution := curationExecution(t, store, run)
	var result worker.Result
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
				var saved worker.Result
				runner.run = func(ctx context.Context, job worker.Job) (worker.Result, error) {
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
				if phase == "apply" {
					wantStatus, wantCalls = "result_pending", 1
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
				if execution.Status != "succeeded" || execution.Resumes != 1 || runner.calls.Load() != wantCalls {
					t.Fatalf("same-run delivery did not settle: status=%s resumes=%d worker_calls=%d", execution.Status, execution.Resumes, runner.calls.Load())
				}
			})
		}
	}
}
