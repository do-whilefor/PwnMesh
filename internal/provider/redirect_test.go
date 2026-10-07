package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"pwnmesh/internal/agent"
)

func TestGenerateDoesNotForwardCredentialsOrPromptsOnRedirect(t *testing.T) {
	var leaked atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"role":"assistant","content":[{"type":"text","text":"redirected"}],"stop_reason":"end_turn"}`))
	}))
	defer destination.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, custom := range []bool{false, true} {
			t.Run(strconv.Itoa(status)+"/custom-client="+strconv.FormatBool(custom), func(t *testing.T) {
				var requests atomic.Int32
				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("x-api-key") != "test-token" {
						t.Error("configured endpoint did not receive its authentication headers")
					}
					http.Redirect(w, r, destination.URL+"/receive", status)
				}))
				defer source.Close()
				p := &Anthropic{BaseURL: source.URL, Token: "test-token"}
				var redirects int
				if custom {
					p.Client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
						redirects++
						return nil
					}}
				}
				_, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "private fixture prompt")}, nil, nil)
				var modelErr *agent.ModelError
				var httpErr *HTTPError
				if !errors.As(err, &modelErr) || modelErr.Kind != agent.ErrorProvider || !errors.As(err, &httpErr) || httpErr.Status != status {
					t.Fatalf("redirect did not fail as an endpoint error: %v", err)
				}
				if requests.Load() != 1 || leaked.Load() != 0 || redirects != 0 {
					t.Fatalf("redirect followed or retried: requests=%d leaked=%d redirects=%d", requests.Load(), leaked.Load(), redirects)
				}
				if custom {
					// The caller may share this client with other integrations.
					if err := p.Client.CheckRedirect(nil, nil); err != nil || redirects != 1 {
						t.Fatal("provider changed the caller's redirect policy")
					}
				}
			})
		}
	}
}
