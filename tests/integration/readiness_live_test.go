//go:build linux

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/config"
	"pwnmesh/internal/dispatcher"
	"pwnmesh/internal/provider"
)

type readinessLiveEvidence struct {
	Scope           string    `json:"scope"`
	Model           string    `json:"model"`
	UpstreamOrigin  string    `json:"upstream_origin"`
	ReasoningEffort string    `json:"reasoning_effort"`
	DeadlineSeconds int       `json:"deadline_seconds"`
	Started         time.Time `json:"started"`
	Finished        time.Time `json:"finished"`
	WallMS          int64     `json:"wall_ms"`
	Passed          bool      `json:"passed"`
	FailureKind     string    `json:"failure_kind,omitempty"`
	HTTPStatus      int       `json:"http_status,omitempty"`
}

// Error strings can contain URLs or other endpoint data. Retain only local
// enumerated failure kinds and HTTP status; never serialize an error or env.
func readinessLiveResult(model, origin string, started, finished time.Time, err error) readinessLiveEvidence {
	evidence := readinessLiveEvidence{
		Scope: "production dispatcher readiness: one no-effect tool call and its result receipt; not task quality or full context capacity",
		Model: model, UpstreamOrigin: origin, ReasoningEffort: "max", DeadlineSeconds: 120,
		Started: started, Finished: finished, WallMS: finished.Sub(started).Milliseconds(), Passed: err == nil,
	}
	if err == nil {
		return evidence
	}
	evidence.FailureKind = "readiness_check_failed"
	var modelError *agent.ModelError
	if errors.As(err, &modelError) {
		switch modelError.Kind {
		case agent.ErrorContextOverflow, agent.ErrorTransport, agent.ErrorRateLimit, agent.ErrorUnavailable, agent.ErrorProvider, agent.ErrorToolArguments, agent.ErrorBudget:
			evidence.FailureKind = string(modelError.Kind)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		evidence.FailureKind = "deadline_exceeded"
	} else if errors.Is(err, context.Canceled) {
		evidence.FailureKind = "cancelled"
	}
	var endpointError *provider.HTTPError
	if errors.As(err, &endpointError) {
		evidence.HTTPStatus = endpointError.Status
	}
	return evidence
}

func TestLiveModelReadiness(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_READINESS_TEST") != "1" {
		t.Skip("opt in with PWNMESH_LIVE_READINESS_TEST=1, model environment and PWNMESH_LIVE_OUTPUT")
	}
	base, token, model := os.Getenv("ANTHROPIC_BASE_URL"), os.Getenv("ANTHROPIC_AUTH_TOKEN"), os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model = os.Getenv("ANTHROPIC_DEFAULT_FABLE_MODEL")
	}
	output := os.Getenv("PWNMESH_LIVE_OUTPUT")
	if strings.TrimSpace(base) == "" || strings.TrimSpace(token) == "" || strings.TrimSpace(model) == "" || strings.TrimSpace(output) == "" {
		t.Fatal("explicit model endpoint, authentication, model selection and PWNMESH_LIVE_OUTPUT are required")
	}
	upstream, err := url.Parse(base)
	if err != nil || upstream.Host == "" || (upstream.Scheme != "https" && upstream.Scheme != "http") || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
		t.Fatal("model endpoint must be an HTTP(S) URL without user info, query or fragment")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal("cannot create readiness evidence directory")
	}
	env := map[string]string{
		"ANTHROPIC_BASE_URL": base, "ANTHROPIC_AUTH_TOKEN": token, "ANTHROPIC_MODEL": model,
		"PWNMESH_REASONING_EFFORT": "max",
	}
	for _, key := range []string{"PWNMESH_MAX_OUTPUT_TOKENS", "PWNMESH_REQUEST_TIMEOUT"} {
		if value := config.Getenv(key); value != "" {
			env[key] = value
		}
	}
	c := config.Config{
		Runtime: config.Runtime{MaxWorkers: 1, HealthMode: "startup_only", HealthTimeout: 120},
		Tasks:   config.Tasks{Reason: config.Task{ReasoningEffort: "max"}},
		Workers: []config.Worker{{Name: "live-readiness", Type: "go", TaskTypes: []string{"reason"}, MaxRunning: 1, Env: env}},
	}
	scheduler := dispatcher.New(c, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	started := time.Now().UTC()
	checkErr := scheduler.Health(ctx, true)
	evidence := readinessLiveResult(model, upstream.Scheme+"://"+upstream.Host, started, time.Now().UTC(), checkErr)
	path := filepath.Join(output, "readiness.json")
	if err := saveLiveJSON(path, evidence); err != nil {
		t.Fatal("cannot save readiness evidence")
	}
	t.Logf("readiness_passed=%t wall_ms=%d failure_kind=%s evidence=%s", evidence.Passed, evidence.WallMS, evidence.FailureKind, path)
	if checkErr != nil {
		t.Fatal("production readiness check failed; inspect the credential-free readiness.json evidence")
	}
}

func TestReadinessLiveEvidenceOmitsProviderErrorDetails(t *testing.T) {
	const secret = "fixture-auth-secret"
	started := time.Unix(0, 0).UTC()
	for _, err := range []error{
		nil,
		errors.New("endpoint failure with " + secret),
		&agent.ModelError{Kind: agent.ErrorProvider, Err: errors.New("response contained " + secret)},
		&agent.ModelError{Kind: agent.ErrorKind(secret), Err: errors.New(secret)},
		&agent.ModelError{Kind: agent.ErrorRateLimit, Err: &provider.HTTPError{Status: 429}},
		context.DeadlineExceeded,
	} {
		evidence := readinessLiveResult("fixture-model", "https://example.invalid", started, started.Add(time.Second), err)
		raw, marshalErr := json.Marshal(evidence)
		if marshalErr != nil || strings.Contains(string(raw), secret) || evidence.Passed != (err == nil) || evidence.WallMS != 1000 {
			t.Fatalf("invalid or unsafe readiness evidence: marshal error=%v", marshalErr)
		}
	}
}
