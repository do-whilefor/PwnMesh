//go:build linux

package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"xloom/internal/agent"
)

// These are stagnation limits, not a limit on useful execution turns. New
// failures can teach the Agent something, so they get a larger allowance than
// repeatedly receiving the exact same observation. Opaque successful commands
// and writes may change external state even when their output is unchanged.
const (
	maxRepeatedToolTurns = 8
	maxFailedToolTurns   = 32
	maxToolObservations  = 128
)

type toolProgress struct {
	Sequence uint64   `json:"sequence,omitempty"`
	Repeated int      `json:"repeated_turns,omitempty"`
	Failed   int      `json:"failed_turns,omitempty"`
	Warned   bool     `json:"warned,omitempty"`
	Seen     []string `json:"observations,omitempty"`
}

type toolProgressFailure struct{ detail string }

func (e *toolProgressFailure) Error() string { return "tool_no_progress: " + e.detail }

func (p toolProgress) problem() error {
	if p.Repeated >= maxRepeatedToolTurns {
		return &toolProgressFailure{fmt.Sprintf("%d consecutive tool turns only repeated unchanged reads or errors; reassess the approach before retrying", p.Repeated)}
	}
	if p.Failed >= maxFailedToolTurns {
		return &toolProgressFailure{fmt.Sprintf("%d consecutive tool turns all failed, despite a correction prompt; reassess the blocker before retrying", p.Failed)}
	}
	return nil
}

func (p toolProgress) needsCorrection() bool {
	return !p.Warned && (p.Repeated >= maxRepeatedToolTurns/2 || p.Failed >= maxFailedToolTurns/2)
}

func (p toolProgress) validate(lastSequence uint64) error {
	if p.Sequence > lastSequence || p.Repeated < 0 || p.Repeated > maxRepeatedToolTurns || p.Failed < 0 || p.Failed > maxFailedToolTurns || len(p.Seen) > maxToolObservations {
		return errors.New("invalid saved tool-progress state")
	}
	if p.Sequence == 0 && (p.Repeated != 0 || p.Failed != 0 || p.Warned || len(p.Seen) != 0) {
		return errors.New("tool-progress state has no settled sequence")
	}
	seen := make(map[string]bool, len(p.Seen))
	for _, fingerprint := range p.Seen {
		raw, err := hex.DecodeString(fingerprint)
		if err != nil || len(raw) != sha256.Size || seen[fingerprint] {
			return errors.New("invalid saved tool observation fingerprint")
		}
		seen[fingerprint] = true
	}
	return nil
}

func readOnlyObservation(name string) bool {
	switch name {
	case "read", "grep", "find", "ls", "read_graph", "read_snapshot", "read_evidence", "cvss31":
		return true
	default:
		return false
	}
}

func toolObservation(call, result agent.Block) (string, error) {
	// Map ordering and insignificant JSON whitespace cannot make an identical
	// call look new. UseNumber preserves integers larger than float64 precision.
	var input any
	decoder := json.NewDecoder(bytes.NewReader(call.Input))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		// Malformed arguments are still a useful failed-call identity.
		input = string(call.Input)
	}
	var output any
	decoder = json.NewDecoder(bytes.NewReader(result.Content))
	decoder.UseNumber()
	if err := decoder.Decode(&output); err != nil {
		output = string(result.Content)
	}
	raw, err := json.Marshal(struct {
		Name   string
		Input  any
		Output any
		Failed bool
	}{call.Name, input, output, result.IsError})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func (p *toolProgress) remember(fingerprint string) bool {
	index := slices.Index(p.Seen, fingerprint)
	if index >= 0 {
		p.Seen = append(slices.Delete(p.Seen, index, index+1), fingerprint)
		return false
	}
	if len(p.Seen) == maxToolObservations {
		p.Seen = slices.Delete(p.Seen, 0, 1)
	}
	p.Seen = append(p.Seen, fingerprint)
	return true
}

// settle runs inside the same atomic session save as the tool results. A crash
// between that save and OnTurnEnd cannot replenish the allowance. The sequence
// makes repeated saves and resume boundaries idempotent. It returns true only
// for successful progress, preserving the existing continuation-budget rule.
func (p *toolProgress) settle(history []agent.Message) (bool, error) {
	if len(history) < 2 {
		return false, nil
	}
	message, results := history[len(history)-2], history[len(history)-1]
	if message.Role != "assistant" || results.Role != "user" || message.Sequence == 0 || message.Sequence <= p.Sequence || !hasToolCalls(message) {
		return false, nil
	}
	var calls []agent.Block
	for _, block := range message.Content {
		if block.Type == "tool_use" {
			calls = append(calls, block)
		}
	}
	if len(results.Content) < len(calls) {
		return false, nil // An instruction is not a settled tool batch.
	}
	for index, call := range calls {
		result := results.Content[index]
		if result.Type != "tool_result" || result.ToolUseID != call.ID {
			return false, nil
		}
	}
	next := *p
	next.Seen = slices.Clone(p.Seen)
	successful, changed, successProgress := false, false, false
	for index, call := range calls {
		result := results.Content[index]
		if !result.IsError {
			successful = true
			if !readOnlyObservation(call.Name) {
				changed, successProgress = true, true
				// A successful mutation invalidates prior reads. Rechecking its
				// effects is useful even when it returns the same file contents.
				next.Seen = nil
				continue
			}
		}
		fingerprint, err := toolObservation(call, result)
		if err != nil {
			return false, err
		}
		if next.remember(fingerprint) {
			changed = true
			if !result.IsError {
				successProgress = true
			}
		}
	}
	next.Sequence = message.Sequence
	if changed {
		next.Repeated = 0
	} else {
		next.Repeated = min(next.Repeated+1, maxRepeatedToolTurns)
	}
	if successful {
		next.Failed = 0
	} else {
		next.Failed = min(next.Failed+1, maxFailedToolTurns)
	}
	if successProgress {
		next.Warned = false
	}
	*p = next
	return successProgress, nil
}
