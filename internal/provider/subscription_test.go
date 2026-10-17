package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pwnmesh/internal/agent"
)

const subscriptionResponse = `{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`

func TestSubscriptionEndpointsArePinnedBeforeResolvingCredentials(t *testing.T) {
	for _, tc := range []struct{ mode, protocol, base string }{
		{"kimi_oauth", "anthropic", "https://custom.invalid/coding"},
		{"kimi_oauth", "openai", "https://api.kimi.com/coding"},
		{"kimi_oauth", "anthropic", "http://api.kimi.com/coding"},
		{"kimi_oauth", "anthropic", "https://api.kimi.com.evil.invalid/coding"},
		{"kimi_oauth", "anthropic", "https://user@api.kimi.com/coding"},
		{"kimi_oauth", "anthropic", "https://api.kimi.com/coding/v1/messages?redirect=1"},
		{"kimi_oauth", "anthropic", "https://api.kimi.com/coding/v1/messages#fragment"},
		{"kimi_oauth", "anthropic", "https://api.kimi.com/other"},
		{"kimi_oauth", "anthropic", ""},
		{"glm_coding_plan", "anthropic", "https://custom.invalid/api/anthropic"},
		{"glm_coding_plan", "openai", "https://open.bigmodel.cn/api/anthropic"},
		{"api_key", "anthropic", "https://api.kimi.com/coding"},
	} {
		t.Run(tc.mode+"/"+tc.protocol+"/"+tc.base, func(t *testing.T) {
			resolved, requests := 0, 0
			p := &Client{AuthMode: tc.mode, Protocol: tc.protocol, BaseURL: tc.base, TokenSource: func(context.Context) (string, error) {
				resolved++
				return "private-token", nil
			}, Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				return nil, errors.New("unexpected network request")
			})}}
			_, err := p.Generate(context.Background(), nil, nil, nil)
			if err == nil || resolved != 0 || requests != 0 || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("untrusted endpoint resolved or received credentials: resolved=%d requests=%d err=%v", resolved, requests, err)
			}
		})
	}
}

func TestKimiOAuthResolvesFreshBearerWithoutChangingSharedClient(t *testing.T) {
	for _, base := range []string{"https://api.kimi.com/coding", "https://api.kimi.com/coding/v1/", "https://api.kimi.com/coding/v1/messages"} {
		t.Run(base, func(t *testing.T) {
			resolved, requests := 0, 0
			messages := []agent.Message{agent.Text("user", "fixture")}
			p := &Client{AuthMode: "kimi_oauth", Protocol: "anthropic", BaseURL: base, Token: "unused-static", Model: "kimi-for-coding", MaxTokens: 1024, ReasoningEffort: "high"}
			p.TokenSource = func(context.Context) (string, error) {
				resolved++
				return fmt.Sprintf("token-%d", resolved), nil
			}
			sizer := &Client{AuthMode: p.AuthMode, Protocol: p.Protocol, Model: p.Model, MaxTokens: p.MaxTokens, ReasoningEffort: p.ReasoningEffort}
			wantBytes, err := sizer.InputBytes(messages, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.InputBytes(messages, nil); err != nil || resolved != 0 {
				t.Fatal("request sizing resolved a private credential", err)
			}
			p.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if req.URL.String() != "https://api.kimi.com/coding/v1/messages" || req.Header.Get("Authorization") != fmt.Sprintf("Bearer token-%d", requests) || req.Header.Get("x-api-key") != "" || req.Header.Get("anthropic-version") != "2023-06-01" {
					t.Error("Kimi OAuth used the wrong endpoint or credential headers")
				}
				raw, _ := io.ReadAll(req.Body)
				var payload map[string]json.RawMessage
				if json.Unmarshal(raw, &payload) != nil || string(payload["thinking"]) != `{"type":"adaptive"}` || string(payload["output_config"]) != `{"effort":"high"}` {
					t.Error("Kimi payload lost its adaptive thinking controls")
				}
				if requests == 1 && len(raw) != wantBytes {
					t.Errorf("public request sizing=%d actual=%d", wantBytes, len(raw))
				}
				return testResponse(req, 200, "application/json", subscriptionResponse), nil
			})}
			if _, err := p.Generate(context.Background(), messages, nil, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := p.GenerateSummary(context.Background(), messages, 128, nil); err != nil {
				t.Fatal(err)
			}
			if requests != 2 || resolved != 2 || p.Token != "unused-static" {
				t.Fatal("successive generations reused or mutated a subscription credential")
			}
		})
	}
}

func TestSubscriptionTokenResolutionHonorsCancellationAndConcurrentCalls(t *testing.T) {
	type tokenKey struct{}
	var calls atomic.Int32
	p := &Client{AuthMode: "kimi_oauth", Protocol: "anthropic", BaseURL: "https://api.kimi.com/coding", Token: "unchanged", TokenSource: func(ctx context.Context) (string, error) {
		calls.Add(1)
		return ctx.Value(tokenKey{}).(string), nil
	}}
	p.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer "+req.Context().Value(tokenKey{}).(string) {
			t.Error("concurrent calls shared resolved credentials")
		}
		return testResponse(req, 200, "application/json", subscriptionResponse), nil
	})}
	var workers sync.WaitGroup
	for _, token := range []string{"first-token", "second-token"} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := p.Generate(context.WithValue(context.Background(), tokenKey{}, token), nil, nil, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Generate(ctx, nil, nil, nil); !errors.Is(err, context.Canceled) || calls.Load() != 2 || p.Token != "unchanged" {
		t.Fatal("canceled call resolved a credential or changed shared state", err)
	}
	p.Timeout = time.Millisecond
	p.TokenSource = func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if _, err := p.Generate(context.Background(), nil, nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("token resolution escaped the request timeout", err)
	}
}

func TestKimiOAuthDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			requests, redirects := 0, 0
			p := &Client{AuthMode: "kimi_oauth", Protocol: "anthropic", BaseURL: "https://api.kimi.com/coding", TokenSource: func(context.Context) (string, error) { return "private-token", nil }}
			p.Client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { redirects++; return nil }, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if requests > 1 || req.URL.Host != "api.kimi.com" {
					t.Error("subscription credentials followed a redirect")
				}
				response := testResponse(req, status, "application/json", `{}`)
				response.Header.Set("Location", "https://custom.invalid/receive")
				return response, nil
			})}
			_, err := p.Generate(context.Background(), nil, nil, nil)
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || httpErr.Status != status || requests != 1 || redirects != 0 {
				t.Fatalf("redirect boundary failed: requests=%d redirects=%d err=%v", requests, redirects, err)
			}
		})
	}
}

func TestGLMCodingPlanUsesProviderThinkingDefaults(t *testing.T) {
	messages := []agent.Message{agent.Text("user", "fixture")}
	p := &Client{AuthMode: "glm_coding_plan", Protocol: "anthropic", BaseURL: "https://open.bigmodel.cn/api/anthropic", Token: "plan-key", Model: "glm-fixture", MaxTokens: 512}
	sizer := &Client{AuthMode: p.AuthMode, Protocol: p.Protocol, Model: p.Model, MaxTokens: p.MaxTokens}
	wantBytes, err := sizer.InputBytes(messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		if req.URL.String() != "https://open.bigmodel.cn/api/anthropic/v1/messages" || req.Header.Get("Authorization") != "Bearer plan-key" || req.Header.Get("x-api-key") != "plan-key" {
			t.Error("GLM plan lost its fixed endpoint or API key authentication")
		}
		if strings.Contains(string(raw), `"thinking"`) || strings.Contains(string(raw), `"output_config"`) || len(raw) != wantBytes {
			t.Error("GLM plan sent unverified effort controls or differs from public sizing")
		}
		return testResponse(req, 200, "application/json", subscriptionResponse), nil
	})}
	if _, err := p.Generate(context.Background(), messages, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestKimiThinkingWithEmptySignatureSurvivesReplay(t *testing.T) {
	stream := "data: {\"type\":\"message_start\"}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"reasoning\",\"signature\":\"\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	message, err := consumeSSE(context.Background(), strings.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &Client{AuthMode: "kimi_oauth", Model: "kimi-for-coding"}
	raw, err := p.payload([]agent.Message{message}, nil, 512)
	if err != nil || !strings.Contains(string(raw), `"thinking":"reasoning","signature":""`) {
		t.Fatal("unsigned Kimi thinking was dropped during replay", err)
	}
}
