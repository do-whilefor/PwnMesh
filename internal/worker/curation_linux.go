//go:build linux

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"xloom/internal/board"
)

const committedCurationText = `{"accepted":true,"data":{"curated":true}}`

// The service receipt, never a final model response, is the authority for a
// completed curation. Recovery checks it before issuing another model call.
type curationCommit struct {
	committed bool
	uncertain bool
	job       Job
	request   func(context.Context, GraphRequest) (string, error)
}

func (c *curationCommit) result() (string, bool) { return committedCurationText, c.committed }

func (c *curationCommit) recover(ctx context.Context) (string, error) {
	raw, err := c.request(ctx, GraphRequest{Op: "curate_receipt"})
	if err != nil {
		return "", err
	}
	var receipt board.StateActionResult
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		return "", err
	}
	c.committed, c.uncertain = receipt.Committed, false
	return raw, nil
}

func (c *curationCommit) action(ctx context.Context, a board.StateAction) (string, error) {
	if c.committed {
		return "", errors.New("curation already committed")
	}
	if c.uncertain {
		raw, err := c.recover(ctx)
		if err != nil || c.committed {
			return raw, err
		}
	}
	if a.Op == "curate" {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(a.Payload, &payload); err != nil || payload == nil {
			return "", errors.New("curate requires an object payload")
		}
		// Only these optional dispute fields have an unambiguous absent value.
		// Keep every other field intact for the existing service validation.
		var groups []map[string]json.RawMessage
		if err := json.Unmarshal(payload["groups"], &groups); err != nil || groups == nil {
			return "", errors.New("curate requires an array of groups")
		}
		for _, group := range groups {
			if group == nil {
				return "", errors.New("curate group must be an object")
			}
			for _, field := range []string{"dispute_id", "review_fact_ids", "resolution"} {
				if bytes.Equal(bytes.TrimSpace(group[field]), []byte("null")) {
					delete(group, field)
				}
			}
		}
		payload["groups"], _ = json.Marshal(groups)
		if raw, exists := payload["through_revision"]; exists {
			var revision int64
			if json.Unmarshal(raw, &revision) != nil || revision != c.job.curationRevision() {
				return "", errors.New("through_revision must match the immutable curation input")
			}
		}
		payload["through_revision"], _ = json.Marshal(c.job.curationRevision())
		a.Payload, _ = json.Marshal(payload)
		a.IdempotencyKey = c.job.RunID + ":curate"
		c.uncertain = true
	} else {
		a.IdempotencyKey = c.job.RunID + ":" + a.IdempotencyKey
	}
	raw, err := c.request(ctx, GraphRequest{Op: "graph_action", Action: a})
	if err != nil {
		return "", err
	}
	if a.Op == "curate" {
		var receipt board.StateActionResult
		if json.Unmarshal([]byte(raw), &receipt) != nil || !receipt.Committed {
			return "", errors.New("curate returned no committed receipt")
		}
		c.committed, c.uncertain = true, false
	}
	return raw, nil
}
