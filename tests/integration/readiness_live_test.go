//go:build linux

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
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
