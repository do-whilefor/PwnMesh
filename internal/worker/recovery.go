//go:build linux

package worker

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/provider"
)

// Only infrastructure failures may continue the same run. A valid declined
// contract is a successful business response and never reaches this function.
func classifyFailure(err error, phase context.Context) (string, bool) {
	if phase.Err() != nil {
		return "budget_exhausted", false
	}
	var model *agent.ModelError
	if errors.As(err, &model) {
		switch model.Kind {
		case agent.ErrorTransport, agent.ErrorRateLimit, agent.ErrorUnavailable:
			return string(model.Kind), true
		default:
			return string(model.Kind), false
		}
	}
	var httpErr *provider.HTTPError
	if errors.As(err, &httpErr) {
		return "provider_http", httpErr.Status == 408 || httpErr.Status == 429 || httpErr.Status >= 500
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "request_timeout", true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return "transport", true
	}
	var network net.Error
	if errors.As(err, &network) && (network.Timeout() || network.Temporary()) {
		return "transport", true
	}
	var format *outputFailure
	var progress *toolProgressFailure
	if errors.As(err, &progress) {
		return "tool_no_progress", false
	}
	if errors.As(err, &format) {
		return "result_contract", false
	}
	return "execution", false
}

// Budget exhaustion alone cannot prove an infrastructure failure. Preserve a
// typed provider cause when the run deadline interrupts a transport operation;
// never infer retry permission from error text or an untyped phase timeout.
func infrastructureFailureCause(err error) string {
	var model *agent.ModelError
	if errors.As(err, &model) {
		return infrastructureCauseKind(string(model.Kind))
	}
	return ""
}

func infrastructureCauseKind(kind string) string {
	switch kind {
	case "transport", "rate_limit", "unavailable", "request_timeout", "transient_infrastructure":
		return kind
	}
	return ""
}
