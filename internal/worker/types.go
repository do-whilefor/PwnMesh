// Package worker executes one isolated task session.
package worker

import "pwnmesh/internal/protocol"

// Runtime signatures use the shared protocol's types and interruption identity.
type Job = protocol.Job
type Result = protocol.Result
type RepairCheck = protocol.RepairCheck
type DecisionMetrics = protocol.DecisionMetrics

var ErrInterrupted = protocol.ErrInterrupted

// DefaultContextTokens is the input allowance for a 1M-token model, leaving
// room for the normal output budget and estimation error. The byte limit is
// a separate request-size guard, not a token conversion.
const (
	DefaultContextTokens = 920000
	// DefaultContextTargetTokens is the estimated input size after compaction.
	DefaultContextTargetTokens = 250000
	DefaultContextBytes        = 8 << 20
)
