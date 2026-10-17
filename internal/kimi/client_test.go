package kimi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Client{BaseURL: server.URL, HTTP: server.Client()}
}

func TestStartUsesOfficialFormAndTrustedCompleteURI(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/oauth/device_authorization" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Accept") != "application/json" {
			t.Error("incorrect device authorization request")
		}
		if r.ParseForm() != nil || r.Form.Get("client_id") != ClientID || len(r.Form) != 1 {
			t.Error("incorrect public client id form")
		}
		io.WriteString(w, `{"device_code":"device","user_code":"CODE","verification_uri":"https://kimi.com/verify","verification_uri_complete":"https://auth.kimi.com/verify?code=CODE","expires_in":900,"interval":5}`)
	})
	device, err := client.Start(context.Background())
	if err != nil || device.DeviceCode != "device" || device.UserCode != "CODE" || device.VerificationURI != "https://auth.kimi.com/verify?code=CODE" || device.ExpiresIn != 900 || device.Interval != 5 {
		t.Fatalf("device login failed: %v", err)
	}
	for _, uri := range []string{"http://kimi.com/verify", "https://evil.invalid/", "https://kimi.com.evil.invalid/", "https://user@kimi.com/", "https://kimi.com:444/", "javascript:alert(1)"} {
		if trustedVerificationURI(uri) {
			t.Errorf("trusted an unsupported verification URI")
		}
	}
}

func TestDevicePollingStatesAndTokenValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body, state string
		status, interval  int
		kind              string
	}{
		{"pending", `{"error":"authorization_pending"}`, "pending", 400, 0, ""},
		{"slow down", `{"error":"slow_down","interval":9}`, "slow_down", 400, 9, ""},
		{"slow down HTTP429", `{"error":"slow_down","interval":10}`, "slow_down", 429, 10, ""},
		{"slow down default", `{"error":"slow_down"}`, "slow_down", 400, 0, ""},
		{"expired", `{"error":"expired_token","error_description":"secret-upstream-body"}`, "expired", 400, 0, "expired"},
		{"denied", `{"error":"access_denied"}`, "denied", 400, 0, "denied"},
		{"authorized", `{"access_token":"access","refresh_token":"refresh","expires_in":3600}`, "authorized", 200, 0, ""},
		{"missing refresh", `{"access_token":"secret-upstream-body","expires_in":3600}`, "error", 200, 0, "invalid_response"},
		{"wrong expiry", `{"access_token":"access","refresh_token":"refresh","expires_in":"3600"}`, "error", 200, 0, "invalid_response"},
		{"overflow expiry", `{"access_token":"access","refresh_token":"refresh","expires_in":999999999999}`, "error", 200, 0, "invalid_response"},
		{"trailing json", `{} {"secret-upstream-body":true}`, "error", 200, 0, "invalid_response"},
		{"too large", strings.Repeat("secret-upstream-body", maxBody/len("secret-upstream-body")+1), "error", 200, 0, "invalid_response"},
		{"limited", `{"error":"rate_limit"}`, "error", 429, 0, "rate_limit"},
		{"unavailable", `secret-upstream-body`, "error", 503, 0, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/api/oauth/token" || r.ParseForm() != nil || r.Form.Get("device_code") != "device" || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("client_id") != ClientID {
					t.Error("incorrect polling form")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			credential, state, interval, err := client.Poll(context.Background(), "device")
			if state != tc.state || interval != tc.interval || calls != 1 {
				t.Fatalf("state=%s interval=%d calls=%d", state, interval, calls)
			}
			if tc.kind == "" && err != nil {
				t.Fatal(err)
			}
			if tc.kind != "" {
				var authErr *Error
				if !errors.As(err, &authErr) || authErr.Kind != tc.kind || strings.Contains(err.Error(), "secret-upstream-body") {
					t.Fatalf("unsafe or incorrect error: %v", err)
				}
			}
			if state == "authorized" && (credential.AccessToken != "access" || credential.RefreshToken != "refresh" || time.Until(credential.ExpiresAt) < 59*time.Minute) {
				t.Fatal("invalid credential")
			}
		})
	}
}

func TestRefreshRotationRetryAndRevocation(t *testing.T) {
	for _, rotated := range []bool{false, true} {
		calls := 0
		client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.ParseForm() != nil || r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != ClientID {
				t.Error("incorrect refresh form")
			}
			if calls == 1 {
				w.WriteHeader(429)
				io.WriteString(w, `{"error":"limited"}`)
				return
			}
			if rotated {
				io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
			} else {
				io.WriteString(w, `{"access_token":"new-access","expires_in":3600}`)
			}
		})
		credential, err := client.Refresh(context.Background(), "old-refresh")
		want := "old-refresh"
		if rotated {
			want = "new-refresh"
		}
		if err != nil || calls != 2 || credential.RefreshToken != want || credential.AccessToken != "new-access" {
			t.Fatalf("refresh failed: calls=%d err=%v", calls, err)
		}
	}
	for _, status := range []int{400, 401, 403} {
		client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			io.WriteString(w, `{"error":"invalid_grant","error_description":"secret-upstream-body"}`)
		})
		if _, err := client.Refresh(context.Background(), "secret-refresh"); !errors.Is(err, ErrLoginRequired) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("revoked credential not safely classified: %v", err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRequestsRejectRedirectsAndRespectCancellationWithoutLeaks(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	})
	if _, err := client.Refresh(context.Background(), "secret-refresh"); err == nil || redirected.Load() != 0 {
		t.Fatal("authorization followed a redirect")
	}
	client.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("secret-refresh secret-upstream-body")
	})}
	if _, err := client.Refresh(context.Background(), "secret-refresh"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("network failure leaked details: %v", err)
	}
	client.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.Refresh(ctx, "secret-refresh"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request ignored deadline: %v", err)
	}
}

func TestStartRejectsMalformedAndUntrustedResponses(t *testing.T) {
	for _, body := range []string{
		`{"device_code":"device","user_code":"CODE","verification_uri":"https://evil.invalid/"}`,
		`{"device_code":"device","user_code":"CODE","verification_uri":"https://kimi.com/","verification_uri_complete":"http://kimi.com/"}`,
		`{"device_code":"device","verification_uri":"https://kimi.com/"}`,
		`{"device_code":"device","user_code":"CODE","verification_uri":"https://kimi.com/","interval":99999999}`,
		`{"device_code":"secret-upstream-body"`,
	} {
		client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
		if _, err := client.Start(context.Background()); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("untrusted authorization response accepted: %v", err)
		}
	}
}
