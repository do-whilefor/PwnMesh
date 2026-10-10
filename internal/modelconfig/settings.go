// Package modelconfig persists operator settings shared by server and dispatcher.
package modelconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"pwnmesh/internal/provider"
)

const PathEnv = "PWNMESH_MODEL_CONFIG_PATH"

type Settings struct {
	Protocol        string `json:"protocol"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	Token           string `json:"token,omitempty"`
	ConnectionMode  string `json:"connection_mode"`
	ProxyURL        string `json:"proxy_url"`
	MaxTokens       int    `json:"max_tokens"`
	RequestTimeout  int    `json:"request_timeout"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type Public struct {
	Settings
	HasToken          bool   `json:"has_token"`
	TokenHint         string `json:"token_hint"`
	AppliesTo         string `json:"applies_to"`
	EffectiveProxyURL string `json:"effective_proxy_url"`
}

func (s Settings) Public() Public {
	hasToken := s.Token != ""
	s.Token = ""
	proxy := ""
	if s.ConnectionMode == "proxy" {
		proxy, _ = provider.ProxyURL(s.ProxyURL, true)
	}
	hint := ""
	if hasToken {
		hint = "••••••••"
	}
	return Public{Settings: s, HasToken: hasToken, TokenHint: hint, AppliesTo: "new_runs", EffectiveProxyURL: proxy}
}

func Defaults(getenv func(string) string) Settings {
	s := Settings{Protocol: "anthropic", BaseURL: getenv("ANTHROPIC_BASE_URL"), Model: getenv("ANTHROPIC_MODEL"), Token: getenv("ANTHROPIC_AUTH_TOKEN"), ConnectionMode: "direct", ProxyURL: "http://host.docker.internal:7897", MaxTokens: provider.DefaultMaxTokens, RequestTimeout: 180, ReasoningEffort: provider.DefaultReasoningEffort}
	if s.BaseURL == "" {
		s.BaseURL = "https://api.deepseek.com/anthropic"
	}
	if s.Model == "" {
		s.Model = getenv("ANTHROPIC_DEFAULT_FABLE_MODEL")
	}
	if s.Model == "" {
		s.Model = "deepseek-flash"
	}
	for key, target := range map[string]*int{"PWNMESH_MAX_OUTPUT_TOKENS": &s.MaxTokens, "PWNMESH_REQUEST_TIMEOUT": &s.RequestTimeout} {
		if n, err := strconv.Atoi(getenv(key)); err == nil && n > 0 {
			*target = n
		}
	}
	if mode := getenv("PWNMESH_CONNECTION_MODE"); mode != "" {
		s.ConnectionMode = mode
	}
	if proxy := getenv("PWNMESH_PROXY_URL"); proxy != "" {
		s.ProxyURL = proxy
	}
	if effort := getenv("PWNMESH_REASONING_EFFORT"); effort != "" {
		s.ReasoningEffort = effort
	}
	return s
}

func (s Settings) Validate(requireToken bool) error {
	if s.Protocol != "anthropic" {
		return errors.New("protocol must be anthropic (Messages API)")
	}
	u, err := url.Parse(s.BaseURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("base_url must be an HTTP(S) URL without credentials, query or fragment")
	}
	if strings.TrimSpace(s.Model) == "" || len(s.Model) > 200 {
		return errors.New("model is required and must not exceed 200 characters")
	}
	if strings.ContainsAny(s.Token, "\r\n\x00") || len(s.Token) > 8192 {
		return errors.New("invalid token")
	}
	if requireToken && strings.TrimSpace(s.Token) == "" {
		return errors.New("token is required")
	}
	if s.ConnectionMode != "direct" && s.ConnectionMode != "proxy" {
		return errors.New("connection_mode must be direct or proxy")
	}
	if s.ProxyURL != "" || s.ConnectionMode == "proxy" {
		if _, err = provider.ProxyURL(s.ProxyURL, false); err != nil {
			return err
		}
	}
	if s.MaxTokens < 1 || s.MaxTokens > 1000000 {
		return errors.New("max_tokens must be between 1 and 1000000")
	}
	if s.RequestTimeout < 1 || s.RequestTimeout > 600 {
		return errors.New("request_timeout must be between 1 and 600 seconds")
	}
	if s.ReasoningEffort != "low" && s.ReasoningEffort != "high" && s.ReasoningEffort != "max" {
		return errors.New("reasoning_effort must be low, high or max")
	}
	return nil
}

// Overlay returns a new map so running tasks retain an immutable configuration.
func (s Settings) Overlay(env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+10)
	for key, value := range env {
		out[key] = value
	}
	for key, value := range map[string]string{"ANTHROPIC_BASE_URL": s.BaseURL, "ANTHROPIC_MODEL": s.Model, "ANTHROPIC_DEFAULT_FABLE_MODEL": s.Model, "ANTHROPIC_AUTH_TOKEN": s.Token, "PWNMESH_CONNECTION_MODE": s.ConnectionMode, "PWNMESH_PROXY_URL": s.ProxyURL, "PWNMESH_MAX_OUTPUT_TOKENS": strconv.Itoa(s.MaxTokens), "PWNMESH_REQUEST_TIMEOUT": strconv.Itoa(s.RequestTimeout), "PWNMESH_REASONING_EFFORT": s.ReasoningEffort} {
		out[key] = value
	}
	return out
}

// Read is atomic with Save's rename. Missing files mean environment fallback.
func Read(path string) (Settings, bool, error) {
	var s Settings
	if path == "" {
		return s, false, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, false, nil
	}
	if err != nil {
		return s, false, errors.New("cannot read model settings")
	}
	if len(raw) > 32<<10 || json.Unmarshal(raw, &s) != nil {
		return s, false, errors.New("invalid model settings file")
	}
	if err = s.Validate(false); err != nil {
		return s, false, fmt.Errorf("invalid model settings: %w", err)
	}
	return s, true, nil
}

type Store struct {
	Path     string
	Defaults Settings
	mu       sync.Mutex
}

func (s *Store) Load() (Settings, error) {
	settings, found, err := Read(s.Path)
	if !found && err == nil {
		settings = s.Defaults
	}
	return settings, err
}

// ReuseToken permits an omitted/empty token only for the same origin. A path,
// model or proxy change may reuse credentials; a different service may not.
// Callers clear Token before applying a patch so omission is distinguishable
// from explicitly supplying a credential for the new endpoint.
func (s *Settings) ReuseToken(previous Settings) error {
	if strings.TrimSpace(s.Token) != "" {
		return nil
	}
	origin := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return ""
		}
		port := 80
		if u.Scheme == "https" {
			port = 443
		}
		if u.Port() != "" {
			port, err = strconv.Atoi(u.Port())
			if err != nil || port < 1 || port > 65535 {
				return ""
			}
		}
		return strings.ToLower(u.Scheme) + "|" + strings.ToLower(u.Hostname()) + "|" + strconv.Itoa(port)
	}
	oldOrigin, newOrigin := origin(previous.BaseURL), origin(s.BaseURL)
	if oldOrigin == "" || newOrigin == "" || oldOrigin != newOrigin {
		return errors.New("base_url origin changed; enter a new token for this service")
	}
	s.Token = previous.Token
	return nil
}

// Update serializes patches, retains same-origin credentials when omitted or
// empty, and replaces the file only after validation and a successful write.
func (s *Store) Update(patch func(*Settings) error) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.Load()
	if err != nil {
		return current, err
	}
	previous := current
	current.Token = ""
	if err = patch(&current); err != nil {
		return current, err
	}
	if err = current.ReuseToken(previous); err != nil {
		return current, err
	}
	if err = current.Validate(false); err != nil {
		return current, err
	}
	if s.Path == "" {
		return current, errors.New("model settings storage is not configured")
	}
	if err = os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return current, errors.New("cannot create model settings directory")
	}
	f, err := os.CreateTemp(filepath.Dir(s.Path), ".model-settings-*")
	if err != nil {
		return current, errors.New("cannot save model settings")
	}
	defer os.Remove(f.Name())
	raw, _ := json.Marshal(current)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), s.Path)
	}
	if err != nil {
		return current, errors.New("cannot save model settings")
	}
	return current, nil
}
