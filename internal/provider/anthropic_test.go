package provider

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
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testProvider(roundTrip roundTripFunc) *Anthropic {
	return &Anthropic{
		BaseURL: "https://example.invalid/step_plan",
		Token:   "test-token",
		Timeout: 5 * time.Second,
		Client:  &http.Client{Transport: roundTrip},
	}
}

func testResponse(req *http.Request, status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestSummaryRequestUsesItsOwnOutputContractWithoutTools(t *testing.T) {
	const prompt = `Return {"notes":"...","quotes":[]}. Original task contract below is context only: {"accepted":true}.`
	p := testProvider(func(req *http.Request) (*http.Response, error) {
		var payload struct {
			System    string             `json:"system"`
			Messages  []agent.Message    `json:"messages"`
			Tools     []agent.Definition `json:"tools"`
			MaxTokens int                `json:"max_tokens"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(payload.System, "current request's output contract") || strings.Contains(payload.System, "task's result contract") || len(payload.Tools) != 0 || payload.MaxTokens != 1024 || len(payload.Messages) != 1 || payload.Messages[0].Text() != prompt {
			t.Fatalf("summary request inherited execution output controls: %+v", payload)
		}
		return testResponse(req, http.StatusOK, "application/json", `{"role":"assistant","content":[{"type":"text","text":"{\"notes\":\"Continue.\",\"quotes\":[]}"}],"stop_reason":"end_turn"}`), nil
	})
	if _, err := p.GenerateSummary(context.Background(), []agent.Message{agent.Text("user", prompt)}, 1024, nil); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateRetriesTransientTransportBeforeResponse(t *testing.T) {
	const body = `{"role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"EOF", io.EOF},
		{"connection reset", syscall.ECONNRESET},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			var payloads []string
			p := testProvider(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.String() != "https://example.invalid/step_plan/v1/messages" {
					t.Errorf("unexpected endpoint: %s", req.URL)
				}
				raw, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				payloads = append(payloads, string(raw))
				if calls == 1 {
					return nil, tc.err
				}
				return testResponse(req, http.StatusOK, "application/json", body), nil
			})
			got, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 2 || got.Text() != "OK" || len(payloads) != 2 || payloads[0] != payloads[1] {
				t.Fatalf("calls=%d, response=%q, request payloads equal=%v", calls, got.Text(), len(payloads) == 2 && payloads[0] == payloads[1])
			}
		})
	}
}

func TestGenerateStopsAfterThreeTransportAttempts(t *testing.T) {
	var calls int
	p := testProvider(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, io.EOF
	})
	_, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
	var modelErr *agent.ModelError
	if calls != 3 || !errors.As(err, &modelErr) || modelErr.Kind != agent.ErrorTransport || !errors.Is(err, io.EOF) {
		t.Fatalf("calls=%d, error=%v", calls, err)
	}
}

func TestGenerateStillRetriesUnavailableHTTPStatus(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls int
			p := testProvider(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return testResponse(req, status, "application/json", `{}`), nil
				}
				return testResponse(req, http.StatusOK, "application/json", `{"role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`), nil
			})
			got, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
			if err != nil || calls != 2 || got.Text() != "OK" {
				t.Fatalf("calls=%d, response=%q, error=%v", calls, got.Text(), err)
			}
		})
	}
}

func TestGenerateBackoffDeadlinePreservesInfrastructureCause(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		cause  error
		kind   agent.ErrorKind
	}{
		{"unavailable", http.StatusServiceUnavailable, nil, agent.ErrorUnavailable},
		{"rate limit", http.StatusTooManyRequests, nil, agent.ErrorRateLimit},
		{"transport", 0, io.EOF, agent.ErrorTransport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := testProvider(func(req *http.Request) (*http.Response, error) {
				calls++
				if tc.cause != nil {
					return nil, tc.cause
				}
				return testResponse(req, tc.status, "application/json", `{}`), nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_, err := p.Generate(ctx, []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
			var modelErr *agent.ModelError
			if calls != 1 || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &modelErr) || modelErr.Kind != tc.kind {
				t.Fatalf("backoff lost its cause or deadline: calls=%d error=%v", calls, err)
			}
			if tc.cause != nil {
				if !errors.Is(err, tc.cause) {
					t.Fatalf("backoff lost transport cause: %v", err)
				}
			} else {
				var httpErr *HTTPError
				if !errors.As(err, &httpErr) || httpErr.Status != tc.status {
					t.Fatalf("backoff lost HTTP status: %v", err)
				}
			}
		})
	}
}

func TestGenerateCancelledBackoffDoesNotReturnInfrastructureFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	p := testProvider(func(req *http.Request) (*http.Response, error) {
		calls++
		cancel()
		return testResponse(req, http.StatusServiceUnavailable, "application/json", `{}`), nil
	})
	_, err := p.Generate(ctx, []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
	var modelErr *agent.ModelError
	if calls != 1 || !errors.Is(err, context.Canceled) || errors.As(err, &modelErr) {
		t.Fatalf("cancelled backoff gained infrastructure retry permission: calls=%d error=%v", calls, err)
	}
}

func TestGenerateRetriesHTTPTimeoutBeforeResponse(t *testing.T) {
	var calls int
	p := testProvider(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return testResponse(req, http.StatusRequestTimeout, "application/json", `{}`), nil
		}
		return testResponse(req, http.StatusOK, "application/json", `{"role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`), nil
	})
	got, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
	if err != nil || calls != 2 || got.Text() != "OK" {
		t.Fatalf("calls=%d, response=%q, error=%v", calls, got.Text(), err)
	}
}

func TestEndpointErrorClassificationPreservesStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   agent.ErrorKind
	}{
		{http.StatusRequestTimeout, agent.ErrorTransport},
		{http.StatusTooManyRequests, agent.ErrorRateLimit},
		{http.StatusServiceUnavailable, agent.ErrorUnavailable},
		{http.StatusBadRequest, agent.ErrorProvider},
		{http.StatusUnauthorized, agent.ErrorProvider},
	} {
		err := classifyEndpointError(tc.status, nil)
		var httpErr *HTTPError
		if err.Kind != tc.kind || !errors.As(err, &httpErr) || httpErr.Status != tc.status {
			t.Fatalf("status=%d error=%+v", tc.status, err)
		}
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestResponseReadFailuresPreserveCauseAndDoNotReplay(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/event-stream"} {
		for _, cause := range []error{syscall.ECONNRESET, io.ErrUnexpectedEOF, context.DeadlineExceeded} {
			t.Run(contentType+"/"+cause.Error(), func(t *testing.T) {
				calls := 0
				p := testProvider(func(req *http.Request) (*http.Response, error) {
					calls++
					res := testResponse(req, http.StatusOK, contentType, "")
					res.Body = io.NopCloser(failingReader{cause})
					return res, nil
				})
				_, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
				var modelErr *agent.ModelError
				if calls != 1 || !errors.As(err, &modelErr) || modelErr.Kind != agent.ErrorTransport || !errors.Is(err, cause) {
					t.Fatalf("calls=%d error=%v", calls, err)
				}
			})
		}
	}
}

func TestMalformedJSONRemainsPermanentAndPreservesCause(t *testing.T) {
	p := testProvider(func(req *http.Request) (*http.Response, error) {
		return testResponse(req, http.StatusOK, "application/json", `{"role":!}`), nil
	})
	_, err := p.Generate(context.Background(), nil, nil, nil)
	var modelErr *agent.ModelError
	var syntax *json.SyntaxError
	if !errors.As(err, &modelErr) || modelErr.Kind != agent.ErrorProvider || !errors.As(err, &syntax) {
		t.Fatalf("malformed JSON was retryable or lost its cause: %v", err)
	}
}

func TestGenerateDoesNotRetryExpiredContext(t *testing.T) {
	var calls int
	p := testProvider(func(req *http.Request) (*http.Response, error) {
		calls++
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := p.Generate(ctx, []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
	if calls != 1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("calls=%d, error=%v", calls, err)
	}
}

func TestGenerateDoesNotRetryPermanentTransportError(t *testing.T) {
	var calls int
	p := testProvider(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("permanent TLS error")
	})
	_, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
	var modelErr *agent.ModelError
	if calls != 1 || !errors.As(err, &modelErr) || modelErr.Kind != agent.ErrorProvider {
		t.Fatalf("calls=%d, error=%v", calls, err)
	}
}

func TestGenerateDoesNotReplayPartialStream(t *testing.T) {
	const stream = "data: {\"type\":\"message_start\"}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
	var calls, deltas int
	p := testProvider(func(req *http.Request) (*http.Response, error) {
		calls++
		return testResponse(req, http.StatusOK, "text/event-stream", stream), nil
	})
	_, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Reply OK")}, nil, func(event agent.Event) {
		if event.Type == "text_delta" {
			deltas++
		}
	})
	if calls != 1 || deltas != 1 || err == nil {
		t.Fatalf("calls=%d, emitted deltas=%d, error=%v", calls, deltas, err)
	}
}

func TestRetryableTransport(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"connection reset", syscall.ECONNRESET, true},
		{"broken pipe", syscall.EPIPE, true},
		{"connection refused", syscall.ECONNREFUSED, true},
		{"request timeout", context.DeadlineExceeded, true},
		{"cancellation", context.Canceled, false},
		{"permanent error", errors.New("invalid certificate"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableTransport(tc.err); got != tc.want {
				t.Fatalf("retryableTransport(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
