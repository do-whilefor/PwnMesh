// Package modelrpc carries credential-free model calls over the worker bridge.
package modelrpc

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/provider"
)

const MaxRequestBytes = 32 << 20
const MaxResponseBytes = 64 << 20

// DeliveryTimeout bounds response publication after the upstream call ends.
const DeliveryTimeout = 30 * time.Second

type Settings struct {
	Model           string        `json:"model"`
	MaxTokens       int           `json:"max_tokens"`
	ReasoningEffort string        `json:"reasoning_effort"`
	Timeout         time.Duration `json:"timeout"`
	Adaptive        bool          `json:"adaptive"`
}

type Request struct {
	RequestID     string             `json:"request_id"`
	SessionID     string             `json:"session_id"`
	Messages      []agent.Message    `json:"messages"`
	Tools         []agent.Definition `json:"tools"`
	SummaryTokens int                `json:"summary_tokens"`
	Deadline      time.Time          `json:"deadline"`
}

type RequestEvent struct {
	Type    string  `json:"type"`
	Request Request `json:"request"`
}

type CancelEvent struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
}

type Response struct {
	RequestID string        `json:"request_id"`
	Message   agent.Message `json:"message"`
	Events    []agent.Event `json:"events"`
	Error     *Error        `json:"error,omitempty"`
}

// Error transmits classifications, never provider response bodies or URLs.
type Error struct {
	Kind       agent.ErrorKind `json:"kind,omitempty"`
	HTTPStatus int             `json:"http_status,omitempty"`
	Canceled   bool            `json:"canceled,omitempty"`
	Deadline   bool            `json:"deadline,omitempty"`
}

func (e *Error) Err() error {
	if e == nil {
		return nil
	}
	var err error = errors.New("dispatcher model request failed")
	if e.HTTPStatus != 0 {
		err = &provider.HTTPError{Status: e.HTTPStatus}
	}
	if e.Canceled {
		err = errors.Join(err, context.Canceled)
	}
	if e.Deadline {
		err = errors.Join(err, context.DeadlineExceeded)
	}
	if e.Kind != "" {
		err = &agent.ModelError{Kind: e.Kind, Err: err}
	}
	return err
}

func ValidRequestID(id string) bool {
	raw, err := hex.DecodeString(id)
	return err == nil && len(raw) == 16 && strings.ToLower(id) == id
}
