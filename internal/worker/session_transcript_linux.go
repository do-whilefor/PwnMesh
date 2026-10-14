//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"pwnmesh/internal/agent"
	eventlog "pwnmesh/internal/worker/internal/journal"
)

// committedTranscript is an identity-validated read boundary. Consumers can
// inspect the saved request view and the committed original message stream,
// without reading session fields or widening the durable journal checkpoint.
type committedTranscript struct {
	history    []agent.Message
	path       string
	checkpoint eventlog.Checkpoint
}

func openCommittedTranscript(runDir string, identity executionIdentity, raw []byte, result Result) (committedTranscript, error) {
	var saved session
	if err := json.Unmarshal(raw, &saved); err != nil {
		return committedTranscript{}, errors.New("published fact requires a readable session")
	}
	if err := saved.validate(identity); err != nil {
		return committedTranscript{}, err
	}
	if saved.Result == nil || saved.Result.Status != "success" || saved.Result.Text != result.Text {
		return committedTranscript{}, errors.New("published fact is not bound to the saved successful result")
	}
	return committedTranscript{history: saved.History, path: filepath.Join(runDir, "events.jsonl"), checkpoint: saved.Log}, nil
}

func (t committedTranscript) visitCurrent(consume func(agent.Message) error) error {
	for _, message := range t.history {
		if err := consume(message); err != nil {
			return err
		}
	}
	return nil
}

func (t committedTranscript) visitOriginal(ctx context.Context, consume func(agent.Message) error) error {
	return eventlog.VisitCommitted(ctx, t.path, t.checkpoint, consume)
}
