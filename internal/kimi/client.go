// Package kimi implements the Kimi Code subscription device login.
package kimi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ClientID       = "17e5f671-d194-4dfb-9706-5516cb48c098"
	OAuthURL       = "https://auth.kimi.com"
	requestTimeout = 30 * time.Second
	maxBody        = 1 << 20
)

// Error deliberately excludes upstream descriptions, bodies and request URLs.
type Error struct {
	Kind   string
	Status int
}

func (e *Error) Error() string {
	switch e.Kind {
	case "login_required":
		return "Kimi Code sign-in is required"
	case "expired":
		return "Kimi Code authorization expired; start sign-in again"
	case "denied":
		return "Kimi Code authorization was denied"
	case "rate_limit":
		return "Kimi Code authorization is rate limited; retry later"
	case "unavailable":
		return "Kimi Code authorization is temporarily unavailable"
	case "network":
		return "Kimi Code authorization connection failed"
	case "invalid_response":
		return "Kimi Code authorization returned an invalid response"
	case "invalid_id":
		return "invalid Kimi Code credential identifier"
	case "storage":
		return "Kimi Code credential storage failed"
	default:
		return "Kimi Code authorization failed"
	}
}

func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && other != nil && e.Kind == other.Kind
}

var ErrLoginRequired = &Error{Kind: "login_required"}

type Device struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresIn       int
	Interval        int
}

type Credential struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type Client struct {
	HTTP *http.Client
	// BaseURL is for isolated tests. Production uses the pinned OAuthURL.
	BaseURL string
}

type response struct {
	DeviceCode           string `json:"device_code"`
	UserCode             string `json:"user_code"`
	VerificationURI      string `json:"verification_uri"`
	VerificationComplete string `json:"verification_uri_complete"`
	ExpiresIn            int    `json:"expires_in"`
	Interval             int    `json:"interval"`
	AccessToken          string `json:"access_token"`
	RefreshToken         string `json:"refresh_token"`
	Error                string `json:"error"`
}

func (c *Client) request(ctx context.Context, path string, values url.Values) (response, int, error) {
	base := c.BaseURL
	if base == "" {
		base = OAuthURL
	}
	values.Set("client_id", ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+path, strings.NewReader(values.Encode()))
	if err != nil {
		return response{}, 0, &Error{Kind: "network"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	bounded := *client
	// Keep local HTTP timeouts transient without masking caller cancellation.
	if bounded.Timeout <= 0 || bounded.Timeout > requestTimeout {
		bounded.Timeout = requestTimeout
	}
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := bounded.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return response{}, 0, ctx.Err()
		}
		return response{}, 0, &Error{Kind: "network"}
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return response{}, res.StatusCode, ctx.Err()
		}
		return response{}, res.StatusCode, &Error{Kind: "network"}
	}
	var data response
	if len(body) > maxBody || json.Unmarshal(body, &data) != nil {
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return response{}, res.StatusCode, statusError(res.StatusCode)
		}
		return response{}, res.StatusCode, &Error{Kind: "invalid_response"}
	}
	return data, res.StatusCode, nil
}

func statusError(status int) error {
	kind := "authorization"
	if status == http.StatusRequestTimeout {
		kind = "network"
	}
	if status == http.StatusTooManyRequests {
		kind = "rate_limit"
	}
	if status >= 500 {
		kind = "unavailable"
	}
	return &Error{Kind: kind, Status: status}
}

func trustedVerificationURI(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "kimi.com", "www.kimi.com", "auth.kimi.com":
		return true
	default:
		return false
	}
}

func (c *Client) Start(ctx context.Context) (Device, error) {
	data, status, err := c.request(ctx, "/api/oauth/device_authorization", url.Values{})
	if err != nil {
		return Device{}, err
	}
	if status < 200 || status >= 300 {
		return Device{}, statusError(status)
	}
	uri := data.VerificationURI
	if data.VerificationComplete != "" {
		uri = data.VerificationComplete
	}
	if strings.TrimSpace(data.DeviceCode) == "" || strings.TrimSpace(data.UserCode) == "" || !trustedVerificationURI(uri) || (data.VerificationURI != "" && !trustedVerificationURI(data.VerificationURI)) || data.Error != "" {
		return Device{}, &Error{Kind: "invalid_response"}
	}
	if data.ExpiresIn <= 0 {
		data.ExpiresIn = 15 * 60
	}
	if data.Interval <= 0 {
		data.Interval = 5
	}
	if data.ExpiresIn > 24*60*60 || data.Interval > 24*60*60 {
		return Device{}, &Error{Kind: "invalid_response"}
	}
	return Device{DeviceCode: data.DeviceCode, UserCode: data.UserCode, VerificationURI: uri, ExpiresIn: data.ExpiresIn, Interval: data.Interval}, nil
}

func token(data response, previousRefresh string) (Credential, error) {
	if data.RefreshToken == "" {
		data.RefreshToken = previousRefresh
	}
	if strings.TrimSpace(data.AccessToken) == "" || strings.TrimSpace(data.RefreshToken) == "" || data.ExpiresIn <= 0 || data.ExpiresIn > 365*24*60*60 || data.Error != "" {
		return Credential{}, &Error{Kind: "invalid_response"}
	}
	return Credential{AccessToken: data.AccessToken, RefreshToken: data.RefreshToken, ExpiresAt: time.Now().Add(time.Duration(data.ExpiresIn) * time.Second)}, nil
}

// Poll never sleeps. The caller enforces the server interval for this device.
func (c *Client) Poll(ctx context.Context, deviceCode string) (Credential, string, int, error) {
	if strings.TrimSpace(deviceCode) == "" {
		return Credential{}, "error", 0, &Error{Kind: "authorization"}
	}
	data, status, err := c.request(ctx, "/api/oauth/token", url.Values{"device_code": {deviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}})
	if err != nil {
		return Credential{}, "error", 0, err
	}
	if status != http.StatusBadRequest && status != http.StatusTooManyRequests && (status < 200 || status >= 300) {
		return Credential{}, "error", 0, statusError(status)
	}
	switch data.Error {
	case "authorization_pending":
		return Credential{}, "pending", 0, nil
	case "slow_down":
		interval := data.Interval
		if interval < 0 || interval > 24*60*60 {
			interval = 0
		}
		return Credential{}, "slow_down", interval, nil
	case "expired_token":
		return Credential{}, "expired", 0, &Error{Kind: "expired"}
	case "access_denied":
		return Credential{}, "denied", 0, &Error{Kind: "denied"}
	}
	if status < 200 || status >= 300 {
		return Credential{}, "error", 0, statusError(status)
	}
	credential, err := token(data, "")
	if err != nil {
		return Credential{}, "error", 0, err
	}
	return credential, "authorized", 0, nil
}

func (c *Client) Refresh(ctx context.Context, refreshToken string) (Credential, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return Credential{}, ErrLoginRequired
	}
	for attempt := 0; ; attempt++ {
		data, status, err := c.request(ctx, "/api/oauth/token", url.Values{"refresh_token": {refreshToken}, "grant_type": {"refresh_token"}})
		if status == 401 || status == 403 || data.Error == "invalid_grant" {
			return Credential{}, ErrLoginRequired
		}
		if attempt == 0 && (status == http.StatusRequestTimeout || status == 429 || status >= 500) {
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return Credential{}, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if err != nil {
			return Credential{}, err
		}
		if status < 200 || status >= 300 {
			return Credential{}, statusError(status)
		}
		return token(data, refreshToken)
	}
}

func validCredential(c Credential) bool {
	return strings.TrimSpace(c.AccessToken) != "" && strings.TrimSpace(c.RefreshToken) != "" && !c.ExpiresAt.IsZero()
}
