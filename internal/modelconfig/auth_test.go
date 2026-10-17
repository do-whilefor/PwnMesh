package modelconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCredentialID = "0123456789abcdef0123456789abcdef"

func subscriptionSettings(mode string) Settings {
	s := Defaults(func(string) string { return "" })
	s.AuthMode = mode
	if mode == "kimi_oauth" {
		s.BaseURL, s.Model, s.CredentialID = "https://api.kimi.com/coding", "kimi-for-coding", testCredentialID
	} else {
		s.BaseURL, s.Model, s.Token = "https://open.bigmodel.cn/api/anthropic", "glm-fixture", "coding-plan-key"
	}
	return s
}

func TestSubscriptionAuthenticationValidationPinsModeAndEndpoint(t *testing.T) {
	for _, mode := range []string{"kimi_oauth", "glm_coding_plan"} {
		valid := subscriptionSettings(mode)
		if err := valid.Validate(true); err != nil {
			t.Fatalf("valid %s settings rejected: %v", mode, err)
		}
		for _, tc := range []struct {
			name   string
			mutate func(*Settings)
		}{
			{"OpenAI protocol", func(s *Settings) { s.Protocol = "openai" }},
			{"other service", func(s *Settings) { s.BaseURL = "https://other.invalid" }},
			{"plaintext endpoint", func(s *Settings) { s.BaseURL = strings.Replace(s.BaseURL, "https:", "http:", 1) }},
			{"extra path", func(s *Settings) { s.BaseURL += "/v1/messages" }},
			{"query", func(s *Settings) { s.BaseURL += "?redirect=elsewhere" }},
			{"unknown mode", func(s *Settings) { s.AuthMode = "subscription" }},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				invalid := valid
				tc.mutate(&invalid)
				if err := invalid.Validate(true); err == nil {
					t.Fatal("unsafe subscription configuration accepted")
				}
			})
		}
	}
	kimi := subscriptionSettings("kimi_oauth")
	kimi.Token = "must-not-fallback"
	if err := kimi.Validate(false); err == nil {
		t.Fatal("Kimi OAuth accepted a manually supplied API token")
	}
	kimi.Token, kimi.CredentialID = "", ""
	if err := kimi.Validate(false); err != nil {
		t.Fatal("unsigned subscription draft rejected")
	}
	if err := kimi.Validate(true); err == nil {
		t.Fatal("unsigned Kimi subscription accepted for execution")
	}
	for _, id := range []string{"../credential", strings.Repeat("x", 32), testCredentialID + "a"} {
		kimi.CredentialID = id
		if err := kimi.Validate(false); err == nil || ValidCredentialID(id) {
			t.Fatal("invalid credential reference accepted")
		}
	}
	glm := subscriptionSettings("glm_coding_plan")
	glm.Token = ""
	if err := glm.Validate(true); err == nil {
		t.Fatal("Coding Plan accepted missing key")
	}
	glm.Token, glm.CredentialID = "plan-key", testCredentialID
	if err := glm.Validate(false); err == nil {
		t.Fatal("Coding Plan accepted an OAuth credential reference")
	}
}

func TestSubscriptionPublicSettingsRedactSecretsAndCredentialLocations(t *testing.T) {
	for _, mode := range []string{"kimi_oauth", "glm_coding_plan"} {
		s := subscriptionSettings(mode)
		s.CredentialDir = "/private/credential-volume"
		public := s.Public()
		raw, err := json.Marshal(public)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{testCredentialID, "coding-plan-key", "/private/credential-volume", `"credential_id"`, `"token":`} {
			if strings.Contains(string(raw), secret) {
				t.Fatal("public settings expose credentials or storage references")
			}
		}
		if !public.HasToken || public.TokenHint != "••••••••" || public.AuthMode != mode || public.CredentialDir != "" || public.CredentialID != "" || public.Token != "" {
			t.Fatal("public credential state is incorrect")
		}
	}
	s := subscriptionSettings("kimi_oauth")
	s.CredentialID = ""
	if s.Public().HasToken || s.Public().TokenHint != "" {
		t.Fatal("unsigned subscription claims credentials")
	}
}

func TestSubscriptionTokenReuseCannotCrossAuthenticationModes(t *testing.T) {
	for _, modes := range [][2]string{{"api_key", "glm_coding_plan"}, {"glm_coding_plan", "api_key"}, {"kimi_oauth", "api_key"}, {"kimi_oauth", "glm_coding_plan"}} {
		previous := subscriptionSettings("glm_coding_plan")
		previous.AuthMode = modes[0]
		if previous.AuthMode == "kimi_oauth" {
			previous.Token, previous.CredentialID = "", testCredentialID
		}
		next := previous
		next.AuthMode, next.Token = modes[1], ""
		if err := next.ReuseToken(previous); err == nil {
			t.Fatalf("credentials crossed auth modes %s -> %s", modes[0], modes[1])
		}
		next.Token = "explicit-new-key"
		if err := next.ReuseToken(previous); err != nil || next.Token != "explicit-new-key" || next.CredentialID != "" || next.CredentialDir != "" {
			t.Fatal("new explicit key did not isolate old OAuth reference")
		}
	}
	previous := subscriptionSettings("glm_coding_plan")
	previous.AuthMode = ""
	next := previous
	next.AuthMode, next.Token = "api_key", ""
	if err := next.ReuseToken(previous); err != nil || next.Token != previous.Token {
		t.Fatal("legacy API key mode stopped retaining same-origin keys")
	}
	previous = subscriptionSettings("glm_coding_plan")
	next = subscriptionSettings("kimi_oauth")
	if err := next.ReuseToken(previous); err != nil || next.Token != "" {
		t.Fatal("Kimi sign-in inherited a manual API key")
	}
}

func TestSubscriptionOverlaySelectsReferenceWithoutOldTokens(t *testing.T) {
	settings := subscriptionSettings("kimi_oauth")
	settings.CredentialDir = "/private/auth"
	env := map[string]string{"ANTHROPIC_AUTH_TOKEN": "old-anthropic", "OPENAI_API_KEY": "old-openai", "ANTHROPIC_CREDENTIAL_ID": "old-reference", "CUSTOM_TOOL": "keep"}
	next := settings.Overlay(env)
	if next["ANTHROPIC_AUTH_MODE"] != "kimi_oauth" || next["ANTHROPIC_CREDENTIAL_ID"] != testCredentialID || next["ANTHROPIC_CREDENTIAL_DIR"] != settings.CredentialDir || next["ANTHROPIC_AUTH_TOKEN"] != "" || next["OPENAI_API_KEY"] != "" || next["CUSTOM_TOOL"] != "keep" || env["ANTHROPIC_AUTH_TOKEN"] != "old-anthropic" {
		t.Fatal("OAuth overlay retained old keys or changed running task environment")
	}
	glm := subscriptionSettings("glm_coding_plan")
	plain := glm.Overlay(next)
	if plain["ANTHROPIC_AUTH_MODE"] != "glm_coding_plan" || plain["ANTHROPIC_AUTH_TOKEN"] != glm.Token || plain["ANTHROPIC_CREDENTIAL_ID"] != "" || plain["ANTHROPIC_CREDENTIAL_DIR"] != "" {
		t.Fatal("Coding Plan overlay retained OAuth credentials")
	}
}

func TestAuthDirectoryAndSettingsPersistenceKeepTokensSeparate(t *testing.T) {
	t.Setenv("PWNMESH_AUTH_DIR", "")
	settingsPath := filepath.Join(t.TempDir(), "config", "models.json")
	t.Setenv(PathEnv, settingsPath)
	want := filepath.Join(filepath.Dir(settingsPath), "model-auth")
	if AuthDir(settingsPath) != want || AuthDir("") != want {
		t.Fatal("auth store did not follow shared settings location")
	}
	store := &Store{Path: settingsPath, Defaults: subscriptionSettings("kimi_oauth")}
	updated, err := store.Update(func(s *Settings) error { s.CredentialDir = "/untrusted/client/location"; return nil })
	if err != nil || updated.CredentialDir != want {
		t.Fatalf("auth storage path was not derived locally: %v", err)
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "client/location") || strings.Contains(string(raw), `"credential_dir"`) || strings.Contains(string(raw), `"token":`) || !strings.Contains(string(raw), testCredentialID) {
		t.Fatal("settings file contains token material or writable credential location")
	}
	loaded, found, err := Read(settingsPath)
	if err != nil || !found || loaded.CredentialDir != want || loaded.CredentialID != testCredentialID || loaded.Token != "" {
		t.Fatalf("OAuth reference did not round trip: %v", err)
	}
	override := filepath.Join(t.TempDir(), "writable-auth-volume")
	t.Setenv("PWNMESH_AUTH_DIR", override)
	if AuthDir(settingsPath) != override || AuthDir("") != override {
		t.Fatal("separate writable auth volume was ignored")
	}
	loaded, _, err = Read(settingsPath)
	if err != nil || loaded.CredentialDir != override {
		t.Fatalf("settings did not use deployment auth path: %v", err)
	}
}
