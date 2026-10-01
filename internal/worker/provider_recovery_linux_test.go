//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/provider"
)

type recoveryRoundTrip func(*http.Request) (*http.Response, error)

func (f recoveryRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type resetResponseReader struct{}

func (resetResponseReader) Read([]byte) (int, error) { return 0, syscall.ECONNRESET }

func TestProviderTransientFailureResumesSameRunWithoutRepeatingTools(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   func() io.ReadCloser
		calls  int
	}{
		{"HTTP timeout", http.StatusRequestTimeout, func() io.ReadCloser { return io.NopCloser(strings.NewReader(`{}`)) }, 3},
		{"JSON connection reset", http.StatusOK, func() io.ReadCloser {
			return io.NopCloser(io.MultiReader(strings.NewReader(`{"role":"assistant","content":[`), resetResponseReader{}))
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, runDir := outcomeJob(t, "explore"), t.TempDir()
			j = bindingSnapshotJob(t, j)
			j.Budget.Timeout = 60
			calls, toolCalls := 0, 0
			recovering := false
			p := &provider.Anthropic{Token: "fixture", BaseURL: "https://example.invalid", Timeout: 10 * time.Second, Client: &http.Client{Transport: recoveryRoundTrip(func(req *http.Request) (*http.Response, error) {
				calls++
				response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Request: req}
				if calls != 1 && !recovering {
					response.StatusCode, response.Body = tc.status, tc.body()
					return response, nil
				}
				message := progressCall(1)
				if recovering {
					message = agent.Text("assistant", completedOutput("explore"))
					message.StopReason = "end_turn"
					payload, err := io.ReadAll(req.Body)
					if err != nil || !strings.Contains(string(payload), "Chunk verified") {
						t.Errorf("recovery lost the completed tool result: err=%v", err)
					}
				}
				raw, _ := json.Marshal(message)
				response.Body = io.NopCloser(strings.NewReader(string(raw)))
				return response, nil
			})}}
			opts := Options{Provider: p, RunDir: runDir, Tools: []agent.Tool{progressTool(&toolCalls, false)}}
			first, err := runTestWorker(context.Background(), j, opts)
			if err != nil || first.Status != "failed" || first.FailureKind != string(agent.ErrorTransport) || !first.Retryable || toolCalls != 1 || calls != tc.calls+1 {
				t.Fatalf("failure cannot recover: result=%+v err=%v tool calls=%d requests=%d", first, err, toolCalls, calls)
			}
			before := outcomeSession(t, runDir)
			assertSnapshotResumeRejectsChangedInput(t, j, opts)
			recovering = true
			last, err := runTestWorker(context.Background(), j, opts)
			after := outcomeSession(t, runDir)
			if err != nil || last.Status != "success" || toolCalls != 1 || calls != tc.calls+2 {
				t.Fatalf("recovery failed or repeated tools: result=%+v err=%v tool calls=%d requests=%d", last, err, toolCalls, calls)
			}
			if after.RunID != before.RunID || after.Identity != before.Identity || !after.ExecutionDeadline.Equal(before.ExecutionDeadline) || after.RecoveryCount != before.RecoveryCount+1 {
				t.Fatalf("recovery changed run identity/budget: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestProviderPermanentFailuresDoNotOfferRunRecovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
	}{
		{name: "permanent transport", err: errors.New("invalid TLS certificate")},
		{name: "authentication", status: http.StatusUnauthorized, body: `{}`},
		{name: "malformed JSON", status: http.StatusOK, body: `{"role":!}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := &provider.Anthropic{Token: "fixture", BaseURL: "https://example.invalid", Client: &http.Client{Transport: recoveryRoundTrip(func(req *http.Request) (*http.Response, error) {
				calls++
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: req}, nil
			})}}
			result, err := runTestWorker(context.Background(), outcomeJob(t, "explore"), Options{Provider: p, RunDir: t.TempDir()})
			if err != nil || result.Status != "failed" || result.FailureKind != string(agent.ErrorProvider) || result.Retryable || calls != 1 {
				t.Fatalf("permanent error offered same-run recovery: result=%+v err=%v requests=%d", result, err, calls)
			}
		})
	}
}

func TestMalformedToolResponseRecoversInsideOriginalWorkerRun(t *testing.T) {
	j, runDir := outcomeJob(t, "explore"), t.TempDir()
	j = bindingSnapshotJob(t, j)
	j.Budget.Timeout = 60
	requests, effects := 0, 0
	p := &provider.Anthropic{Token: "fixture", BaseURL: "https://example.invalid", Client: &http.Client{Transport: recoveryRoundTrip(func(req *http.Request) (*http.Response, error) {
		requests++
		response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Request: req}
		if requests == 2 || requests == 4 {
			response.Header.Set("Content-Type", "text/event-stream")
			response.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"bad\",\"name\":\"progress\",\"input\":{}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"))
			return response, nil
		}
		message := progressCall(requests)
		if requests > 2 {
			state := outcomeSession(t, runDir)
			if state.ContextCheckpoint.ToolArgumentRetries != 1 || state.RecoveryCount != 0 || state.RunID != j.RunID {
				t.Fatalf("retry lost its durable bound or restarted the Worker: %+v", state)
			}
			payload, err := io.ReadAll(req.Body)
			if err != nil || !strings.Contains(string(payload), "Chunk verified") {
				t.Fatalf("lost prior tool receipt: %v", err)
			}
			if requests == 5 {
				message = agent.Text("assistant", completedOutput("explore"))
				message.StopReason = "end_turn"
			}
		}
		raw, _ := json.Marshal(message)
		response.Body = io.NopCloser(strings.NewReader(string(raw)))
		return response, nil
	})}}
	opts := Options{Provider: p, RunDir: runDir, Tools: []agent.Tool{progressTool(&effects, false)}}
	result, err := runTestWorker(context.Background(), j, opts)
	if err != nil || result.Status != "success" || requests != 5 || effects != 2 {
		t.Fatalf("in-run recovery failed/replayed tools: result=%+v err=%v requests=%d effects=%d", result, err, requests, effects)
	}
	state := outcomeSession(t, runDir)
	if state.ContextCheckpoint.ToolArgumentRetries != 1 || state.RecoveryCount != 0 {
		t.Fatalf("retry allowance was not retained: %+v", state)
	}
}
