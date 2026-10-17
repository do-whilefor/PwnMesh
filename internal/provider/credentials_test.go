package provider

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/kimi"
	"pwnmesh/internal/modelwire"
)

func TestKimiConfigurationReadHonorsCancellation(t *testing.T) {
	id := strings.Repeat("c", 32)
	store := &kimi.Store{Dir: t.TempDir()}
	if err := store.Save(id, kimi.Credential{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(store.Dir, id+".lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	var requests atomic.Int32
	p := &Client{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected network request")
	})}}
	env := map[string]string{
		"ANTHROPIC_AUTH_MODE": "kimi_oauth", "ANTHROPIC_BASE_URL": "https://api.kimi.com/coding",
		"ANTHROPIC_CREDENTIAL_DIR": store.Dir, "ANTHROPIC_CREDENTIAL_ID": id,
	}
	for name, load := range map[string]func(context.Context) error{
		"credential setup": func(ctx context.Context) error { return p.configureKimi(ctx, store.Dir, id) },
		"environment setup": func(ctx context.Context) error {
			_, err := FromEnvironment(ctx, func(key string) string { return env[key] }, "")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- load(ctx) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) || requests.Load() != 0 || p.Token != "" || p.TokenSource != nil {
					t.Fatal("cancelled credential setup read a token or used the network", err)
				}
			case <-time.After(time.Second):
				// Release the external lock before failing so the loader cannot leak.
				unix.Flock(int(lock.Fd()), unix.LOCK_UN)
				<-done
				t.Fatal("credential setup ignored cancellation while waiting for the lock")
			}
		})
	}
}

func TestKimiCredentialRefreshRetainsRetryClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		kind   agent.ErrorKind
	}{
		{"network", 0, agent.ErrorTransport},
		{"request_timeout", 408, agent.ErrorTransport},
		{"rate_limit", 429, agent.ErrorRateLimit},
		{"unavailable", 503, agent.ErrorUnavailable},
		{"revoked", 401, agent.ErrorProvider},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := strings.Repeat("a", 32)
			store := &kimi.Store{Dir: t.TempDir()}
			if err := store.Save(id, kimi.Credential{AccessToken: "private-access", RefreshToken: "private-refresh", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
			requests := 0
			p := &Client{Protocol: "anthropic", AuthMode: "kimi_oauth", BaseURL: "https://api.kimi.com/coding", Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if req.URL.String() != "https://auth.kimi.com/api/oauth/token" {
					t.Fatal("refresh failure reached the model endpoint")
				}
				if tc.status == 0 {
					return nil, errors.New("network failure containing private-refresh")
				}
				return testResponse(req, tc.status, "application/json", `{"error":"upstream-private-access-private-refresh"}`), nil
			})}}
			if err := p.configureKimi(context.Background(), store.Dir, id); err != nil {
				t.Fatal(err)
			}
			_, err := p.Generate(context.Background(), nil, nil, nil)
			var modelErr *agent.ModelError
			if !errors.As(err, &modelErr) || modelErr.Kind != tc.kind || strings.Contains(err.Error(), "private-") || requests == 0 {
				t.Fatalf("refresh classification or redaction failed: %v", err)
			}
			if tc.status == 408 || tc.status == 429 || tc.status == 503 {
				var endpoint *modelwire.HTTPError
				if !errors.As(err, &endpoint) || endpoint.Status != tc.status || requests != 2 {
					t.Fatal("refresh HTTP status was lost", err)
				}
			}
			_, storedErr := store.Read(context.Background(), id)
			if tc.status == 401 {
				if !errors.Is(err, kimi.ErrLoginRequired) || !errors.Is(storedErr, kimi.ErrLoginRequired) {
					t.Fatal("revoked credentials remained usable", err, storedErr)
				}
			} else if storedErr != nil {
				t.Fatal("transient error deleted the saved credential", storedErr)
			}
		})
	}
}

func TestKimiCredentialRotationRetainsEverySecretForRedaction(t *testing.T) {
	id := strings.Repeat("b", 32)
	store := &kimi.Store{Dir: t.TempDir()}
	initial := kimi.Credential{AccessToken: "initial-access", RefreshToken: "initial-refresh", ExpiresAt: time.Now().Add(-time.Minute)}
	if err := store.Save(id, initial); err != nil {
		t.Fatal(err)
	}
	p := &Client{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return testResponse(req, 200, "application/json", `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`), nil
	})}}
	if err := p.configureKimi(context.Background(), store.Dir, id); err != nil {
		t.Fatal(err)
	}
	token, err := p.TokenSource(context.Background())
	if err != nil || token != "rotated-access" || p.Token != initial.AccessToken {
		t.Fatal("credential rotation changed shared static state", err)
	}
	for _, secret := range []string{"initial-access", "initial-refresh", "rotated-access", "rotated-refresh"} {
		found := false
		for _, value := range p.CredentialValues() {
			found = found || value == secret
		}
		if !found {
			t.Fatal("credential rotation dropped a secret from response redaction")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.TokenSource(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("credential source changed cancellation classification", err)
	}
}
