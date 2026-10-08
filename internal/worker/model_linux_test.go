//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"pwnmesh/internal/agent"
	"pwnmesh/internal/config"
	"pwnmesh/internal/modelrpc"
	"pwnmesh/internal/provider"
)

type modelBridgeWriter func([]byte) (int, error)

func (f modelBridgeWriter) Write(raw []byte) (int, error) { return f(raw) }

func modelSettingsFixture(t *testing.T, dir string) modelrpc.Settings {
	t.Helper()
	settings := modelrpc.Settings{Model: "fixture-model", MaxTokens: 8192, ReasoningEffort: "high", Timeout: time.Second, Adaptive: true}
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "model-settings.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return settings
}

func modelResponseFixture(dir string, response modelrpc.Response) error {
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	name := filepath.Join(dir, "model-response-"+response.RequestID+".json")
	if err = os.WriteFile(name+".tmp", raw, 0600); err != nil {
		return err
	}
	return os.Rename(name+".tmp", name)
}

func TestModelForJobUsesOnlyPublicSettingsAndPersistedEffort(t *testing.T) {
	dir := t.TempDir()
	settings := modelSettingsFixture(t, dir)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "unused-fixture-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://unused.invalid/private")
	t.Setenv("ANTHROPIC_MODEL", "ignored-model")
	t.Setenv("PWNMESH_REASONING_EFFORT", "low")
	j := Job{RunID: "saved-run", Budget: config.Task{ReasoningEffort: "high"}}
	p, err := modelForJob(j, Options{RunDir: dir, Output: io.Discard}, "")
	if err != nil {
		t.Fatal(err)
	}
	var _ agent.Provider = p
	var _ agent.SummaryProvider = p
	var _ agent.RequestSizer = p
	if p.sizer.Token != "" || p.sizer.BaseURL != "https://openrouter.ai" || p.sizer.Model != settings.Model || p.sessionID != j.RunID || p.timeout != settings.Timeout {
		t.Fatal("bridge used private environment settings or lost its session policy")
	}
	history := []agent.Message{agent.Text("user", "fixture")}
	definitions := []agent.Definition{{Name: "fixture", Schema: json.RawMessage(`{"type":"object"}`)}}
	expected := &provider.Anthropic{BaseURL: "https://openrouter.ai/api", Model: settings.Model, MaxTokens: settings.MaxTokens, ReasoningEffort: settings.ReasoningEffort}
	want, err := expected.InputBytes(history, definitions)
	got, gotErr := p.InputBytes(history, definitions)
	if err != nil || gotErr != nil || got != want {
		t.Fatalf("request sizing changed: got=%d want=%d errors=%v/%v", got, want, gotErr, err)
	}
	j.Budget.ReasoningEffort = "low"
	if _, err = modelForJob(j, Options{RunDir: dir, Output: io.Discard}, ""); err == nil {
		t.Fatal("bridge accepted a changed persisted role policy")
	}
}

func TestModelForJobRejectsMissingAndUnsafeSettings(t *testing.T) {
	for _, mode := range []string{"missing", "symlink", "directory", "fifo", "oversized", "invalid JSON", "invalid policy"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			name := filepath.Join(dir, "model-settings.json")
			var err error
			switch mode {
			case "symlink":
				target := t.TempDir()
				modelSettingsFixture(t, target)
				err = os.Symlink(filepath.Join(target, "model-settings.json"), name)
			case "directory":
				err = os.Mkdir(name, 0700)
			case "fifo":
				err = unix.Mkfifo(name, 0600)
			case "oversized":
				err = os.WriteFile(name, []byte(strings.Repeat(" ", (64<<10)+1)), 0600)
			case "invalid JSON":
				err = os.WriteFile(name, []byte("{"), 0600)
			case "invalid policy":
				err = os.WriteFile(name, []byte(`{"max_tokens":8192,"reasoning_effort":"high","timeout":0}`), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = modelForJob(Job{RunID: "fixture"}, Options{RunDir: dir, Output: io.Discard}, ""); err == nil {
				t.Fatal("unsafe or absent settings were accepted")
			}
		})
	}
}

func TestWorkerCLIRequiresCredentialFreeDispatcherBeforeSession(t *testing.T) {
	for _, mode := range []string{"no bridge", "auth token", "API key", "endpoint", "lowercase prefix", "mixed prefix"} {
		t.Run(mode, func(t *testing.T) {
			for _, entry := range os.Environ() {
				key, _, _ := strings.Cut(entry, "=")
				if strings.HasPrefix(strings.ToUpper(key), "ANTHROPIC_") {
					t.Setenv(key, "")
				}
			}
			t.Setenv("PWNMESH_MODEL_BRIDGE", "dispatcher-v1")
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
			t.Setenv("ANTHROPIC_API_KEY", "")
			if mode == "no bridge" {
				t.Setenv("PWNMESH_MODEL_BRIDGE", "")
			} else if mode == "auth token" {
				t.Setenv("ANTHROPIC_AUTH_TOKEN", "fixture-secret")
			} else if mode == "API key" {
				t.Setenv("ANTHROPIC_API_KEY", "fixture-secret")
			} else if mode == "endpoint" {
				t.Setenv("ANTHROPIC_BASE_URL", "https://fixture-secret@example.invalid")
			} else if mode == "lowercase prefix" {
				t.Setenv("anthropic_auth_token", "fixture-secret")
			} else {
				t.Setenv("Anthropic_Model", "fixture-secret")
			}
			dir := t.TempDir()
			err := Execute(context.Background(), filepath.Join(dir, "missing-job.json"), io.Discard)
			if err == nil || os.IsNotExist(err) || strings.Contains(err.Error(), "fixture-secret") {
				t.Fatalf("CLI did not fail safely before accessing the job: %v", err)
			}
			files, _ := os.ReadDir(dir)
			if len(files) != 0 {
				t.Fatal("rejected CLI created session state")
			}
		})
	}
}

func TestModelBridgeLargeRequestSummaryAndPartialTypedErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failure   *modelrpc.Error
		retryable bool
	}{
		{"success", nil, false},
		{"rate limit", &modelrpc.Error{Kind: agent.ErrorRateLimit, HTTPStatus: 429}, true},
		{"unavailable", &modelrpc.Error{Kind: agent.ErrorUnavailable, HTTPStatus: 503}, true},
		{"authentication", &modelrpc.Error{Kind: agent.ErrorProvider, HTTPStatus: 401}, false},
		{"context overflow", &modelrpc.Error{Kind: agent.ErrorContextOverflow, HTTPStatus: 400}, false},
		{"tool arguments", &modelrpc.Error{Kind: agent.ErrorToolArguments}, false},
		{"transport deadline", &modelrpc.Error{Kind: agent.ErrorTransport, Deadline: true}, true},
		{"cancel", &modelrpc.Error{Canceled: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			modelSettingsFixture(t, dir)
			history := []agent.Message{agent.Text("user", strings.Repeat("large fixture ", 20000))}
			calls, emitted := 0, 0
			var lastRequest modelrpc.Request
			writer := modelBridgeWriter(func(raw []byte) (int, error) {
				var event modelrpc.RequestEvent
				if err := json.Unmarshal(raw, &event); err != nil {
					return 0, err
				}
				if event.Type != "model_request" || !modelrpc.ValidRequestID(event.Request.RequestID) || len(raw) <= 128<<10 {
					return 0, errors.New("invalid large model request")
				}
				calls++
				lastRequest = event.Request
				message := agent.Text("assistant", "retained partial output")
				message.Usage = &agent.Usage{InputTokens: 123, OutputTokens: 7}
				message.StopReason = "end_turn"
				err := modelResponseFixture(dir, modelrpc.Response{RequestID: event.Request.RequestID, Message: message, Events: []agent.Event{{Type: "text_delta", Text: "retained partial output"}}, Error: tc.failure})
				return len(raw), err
			})
			p, err := modelForJob(Job{RunID: "run"}, Options{RunDir: dir, Output: writer}, "run:graph:child")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			deadline, _ := ctx.Deadline()
			message, err := p.GenerateSummary(ctx, history, 16384, func(event agent.Event) {
				if event.Type == "text_delta" {
					emitted++
				}
			})
			if calls != 1 || emitted != 1 || lastRequest.SessionID != "run:graph:child" || lastRequest.SummaryTokens != 8192 || len(lastRequest.Tools) != 0 || !lastRequest.Deadline.Equal(deadline) {
				t.Fatal("bridge lost the session, summary allowance, events or phase deadline")
			}
			if message.Text() != "retained partial output" || message.Usage == nil || message.Usage.InputTokens != 123 {
				t.Fatal("bridge discarded partial message or usage")
			}
			if tc.failure == nil {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("bridge discarded model failure")
				}
				if tc.failure.Kind != "" {
					var model *agent.ModelError
					if !errors.As(err, &model) || model.Kind != tc.failure.Kind {
						t.Fatal("model error classification was lost")
					}
				}
				if tc.failure.HTTPStatus != 0 {
					var httpErr *provider.HTTPError
					if !errors.As(err, &httpErr) || httpErr.Status != tc.failure.HTTPStatus {
						t.Fatal("provider status was lost")
					}
				}
				if errors.Is(err, context.Canceled) != tc.failure.Canceled || errors.Is(err, context.DeadlineExceeded) != tc.failure.Deadline {
					t.Fatal("context error semantics changed")
				}
				_, retry := classifyFailure(err, context.Background())
				if retry != tc.retryable {
					t.Fatal("bridge changed same-run recovery permission")
				}
			}
			if _, err = os.Stat(filepath.Join(dir, "model-response-"+lastRequest.RequestID+".json")); !os.IsNotExist(err) {
				t.Fatal("consumed response was retained")
			}
		})
	}
}

func TestModelBridgeMissingResponseUsesPhaseDeadlineAndCancels(t *testing.T) {
	dir := t.TempDir()
	modelSettingsFixture(t, dir)
	var request modelrpc.RequestEvent
	var canceled modelrpc.CancelEvent
	writer := modelBridgeWriter(func(raw []byte) (int, error) {
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return 0, err
		}
		if envelope.Type == "model_request" {
			return len(raw), json.Unmarshal(raw, &request)
		}
		if envelope.Type == "model_cancel" {
			return len(raw), json.Unmarshal(raw, &canceled)
		}
		return 0, errors.New("unexpected event")
	})
	p, err := modelForJob(Job{RunID: "run"}, Options{RunDir: dir, Output: writer}, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = p.Generate(ctx, []agent.Message{agent.Text("user", "fixture")}, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !modelrpc.ValidRequestID(request.Request.RequestID) || canceled.RequestID != request.Request.RequestID {
		t.Fatalf("missing response did not cancel the request: %v", err)
	}
	deadline, _ := ctx.Deadline()
	if time.Since(started) < modelrpc.DeliveryTimeout || time.Now().Before(deadline.Add(modelrpc.DeliveryTimeout)) {
		t.Fatal("phase deadline did not allow the bounded diagnostic delivery window")
	}
}

func TestModelBridgePhaseDeadlineRetainsOnlyCurrentTypedFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure *modelrpc.Error
		cause   string
	}{
		{"transport", &modelrpc.Error{Kind: agent.ErrorTransport, Deadline: true}, "transport"},
		{"rate limit", &modelrpc.Error{Kind: agent.ErrorRateLimit, HTTPStatus: 429}, "rate_limit"},
		{"unavailable", &modelrpc.Error{Kind: agent.ErrorUnavailable, HTTPStatus: 503}, "unavailable"},
		{"authentication", &modelrpc.Error{Kind: agent.ErrorProvider, HTTPStatus: 401}, ""},
		{"HTTP only", &modelrpc.Error{HTTPStatus: 503}, ""},
		{"untyped deadline", &modelrpc.Error{Deadline: true}, ""},
		{"late success", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			modelSettingsFixture(t, dir)
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			requests, cancels, emitted := 0, 0, 0
			writer := modelBridgeWriter(func(raw []byte) (int, error) {
				var event modelrpc.RequestEvent
				if err := json.Unmarshal(raw, &event); err != nil {
					return 0, err
				}
				if event.Type == "model_cancel" {
					cancels++
					return len(raw), nil
				}
				requests++
				// Publish only after the actual phase context has expired. This
				// makes the late-response ordering independent of timer races.
				<-ctx.Done()
				message := agent.Message{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "late", Name: "must_not_run", Input: json.RawMessage(`{}`)}}, StopReason: "tool_use"}
				err := modelResponseFixture(dir, modelrpc.Response{RequestID: event.Request.RequestID, Message: message, Events: []agent.Event{{Type: "tool_delta", Text: "late tool"}}, Error: tc.failure})
				return len(raw), err
			})
			p, err := modelForJob(Job{RunID: "run"}, Options{RunDir: dir, Output: writer}, "")
			if err != nil {
				t.Fatal(err)
			}
			message, err := p.Generate(ctx, nil, nil, func(agent.Event) { emitted++ })
			if !errors.Is(err, context.DeadlineExceeded) || len(message.Content) != 0 || emitted != 0 || requests != 1 || cancels != 0 {
				t.Fatalf("late response extended execution or raced dispatcher cancellation: requests=%d cancels=%d events=%d error=%v", requests, cancels, emitted, err)
			}
			kind, retryable := classifyFailure(err, ctx)
			if kind != "budget_exhausted" || retryable || infrastructureFailureCause(err) != tc.cause {
				t.Fatalf("late response changed exhaustion diagnosis: kind=%q retryable=%v cause=%q", kind, retryable, infrastructureFailureCause(err))
			}
			if tc.failure != nil && tc.failure.HTTPStatus != 0 {
				var httpErr *provider.HTTPError
				if !errors.As(err, &httpErr) || httpErr.Status != tc.failure.HTTPStatus {
					t.Fatal("late response lost HTTP classification")
				}
			}
			if _, err = p.Generate(ctx, nil, nil, nil); !errors.Is(err, context.DeadlineExceeded) || requests != 1 {
				t.Fatal("expired phase started another model request")
			}
		})
	}
}

func TestModelBridgeExternalCancellationDoesNotDrainOrAuthorizeRecovery(t *testing.T) {
	dir := t.TempDir()
	modelSettingsFixture(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests, cancels, emitted := 0, 0, 0
	writer := modelBridgeWriter(func(raw []byte) (int, error) {
		var event modelrpc.RequestEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return 0, err
		}
		if event.Type == "model_cancel" {
			cancels++
			return len(raw), nil
		}
		requests++
		cancel()
		// Even an available typed failure must not turn external cancellation
		// into permission to recover automatically.
		err := modelResponseFixture(dir, modelrpc.Response{RequestID: event.Request.RequestID, Error: &modelrpc.Error{Kind: agent.ErrorTransport}, Events: []agent.Event{{Type: "text_delta", Text: "late"}}})
		return len(raw), err
	})
	p, err := modelForJob(Job{RunID: "run"}, Options{RunDir: dir, Output: writer}, "")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	message, err := p.Generate(ctx, nil, nil, func(agent.Event) { emitted++ })
	if !errors.Is(err, context.Canceled) || infrastructureFailureCause(err) != "" || len(message.Content) != 0 || emitted != 0 || requests != 1 || cancels != 1 || time.Since(started) >= time.Second {
		t.Fatalf("external cancellation acquired a drain window or recovery cause: error=%v requests=%d cancels=%d events=%d", err, requests, cancels, emitted)
	}
}

func TestModelBridgeProviderTimeoutAllowsTypedFailureDelivery(t *testing.T) {
	dir := t.TempDir()
	modelSettingsFixture(t, dir)
	delivered := make(chan error, 1)
	writer := modelBridgeWriter(func(raw []byte) (int, error) {
		var event modelrpc.RequestEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return 0, err
		}
		go func() {
			// The provider's timeout has expired, but its classified failure
			// still needs to cross the response file bridge.
			time.Sleep(40 * time.Millisecond)
			delivered <- modelResponseFixture(dir, modelrpc.Response{RequestID: event.Request.RequestID, Error: &modelrpc.Error{Kind: agent.ErrorUnavailable, HTTPStatus: 503, Deadline: true}})
		}()
		return len(raw), nil
	})
	p, err := modelForJob(Job{RunID: "run"}, Options{RunDir: dir, Output: writer}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.timeout = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = p.Generate(ctx, nil, nil, nil)
	if deliveryErr := <-delivered; deliveryErr != nil {
		t.Fatal(deliveryErr)
	}
	var model *agent.ModelError
	if !errors.As(err, &model) || model.Kind != agent.ErrorUnavailable || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bridge timeout hid the provider's infrastructure cause: %v", err)
	}
}

func TestModelBridgeRejectsUnsafeOrMismatchedResponses(t *testing.T) {
	for _, mode := range []string{"symlink", "fifo", "oversized", "wrong request"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			modelSettingsFixture(t, dir)
			writer := modelBridgeWriter(func(raw []byte) (int, error) {
				var event modelrpc.RequestEvent
				if err := json.Unmarshal(raw, &event); err != nil {
					return 0, err
				}
				name := filepath.Join(dir, "model-response-"+event.Request.RequestID+".json")
				var err error
				switch mode {
				case "symlink":
					err = os.Symlink(filepath.Join(dir, "model-settings.json"), name)
				case "fifo":
					err = unix.Mkfifo(name, 0600)
				case "oversized":
					var file *os.File
					file, err = os.Create(name)
					if err == nil {
						err = errors.Join(file.Truncate(modelrpc.MaxResponseBytes+1), file.Close())
					}
				case "wrong request":
					err = os.WriteFile(name, []byte(`{"request_id":"other"}`), 0600)
				}
				return len(raw), err
			})
			p, err := modelForJob(Job{RunID: "run"}, Options{RunDir: dir, Output: writer}, "")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err = p.Generate(ctx, nil, nil, nil); err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("unsafe response was accepted or blocked reading: %v", err)
			}
		})
	}
}

func TestModelBridgeConcurrentChildRequestsStayIsolated(t *testing.T) {
	dir := t.TempDir()
	modelSettingsFixture(t, dir)
	requests := make(chan modelrpc.Request, 2)
	writer := &graphResultWriter{writer: modelBridgeWriter(func(raw []byte) (int, error) {
		var event modelrpc.RequestEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return 0, err
		}
		requests <- event.Request
		return len(raw), nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errorsFound := make(chan error, 2)
	for _, node := range []string{"one", "two"} {
		p, err := modelForJob(Job{RunID: "run"}, Options{RunDir: dir, Output: writer}, "run:graph:"+node)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			message, err := p.Generate(ctx, []agent.Message{agent.Text("user", p.sessionID)}, nil, nil)
			if err == nil && message.Text() != p.sessionID {
				err = fmt.Errorf("child received another session's response")
			}
			errorsFound <- err
		}()
	}
	var received []modelrpc.Request
	for len(received) < 2 {
		select {
		case request := <-requests:
			received = append(received, request)
		case <-ctx.Done():
			t.Fatal("child model calls did not both reach the bridge")
		}
	}
	if received[0].RequestID == received[1].RequestID || received[0].SessionID == received[1].SessionID {
		t.Fatal("concurrent requests share identity")
	}
	// Publish in the opposite order to prove responses bind to each request.
	for i := len(received) - 1; i >= 0; i-- {
		request := received[i]
		if err := modelResponseFixture(dir, modelrpc.Response{RequestID: request.RequestID, Message: agent.Text("assistant", request.SessionID)}); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
}
