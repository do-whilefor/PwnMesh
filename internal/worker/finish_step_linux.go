//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"xloom/internal/agent"
	"xloom/internal/process"
)

// This is an explicit model-authored handoff, not a command-success shortcut.
// The existing final-result path freezes evidence, and the Dispatcher retains
// authority to accept the Step and publish its fact under the current lease.
type stepFinish struct {
	job    Job
	runDir string
	text   string
}

type stepFinishReceipt struct {
	Identity executionIdentity `json:"identity"`
	Text     string            `json:"text"`
}

func (f *stepFinish) result() (string, bool) { return f.text, f.text != "" }

func (f *stepFinish) check(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := process.CheckLaunch(f.runDir); err != nil {
		return err
	}
	if process.Cancelled(f.runDir) {
		return context.Canceled
	}
	parsed, err := parseOutput(f.job, false, text)
	if err != nil {
		return err
	}
	if parsed.Outcome != "completed" {
		return errors.New("finish_step requires an explicit completed observation with original evidence")
	}
	if parsed.FactID != "" {
		return f.checkPublished(ctx, text, parsed.FactID)
	}
	var fact struct {
		ObservedAt string `json:"observed_at"`
	}
	if err := json.Unmarshal(parsed.FactPayload, &fact); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339Nano, fact.ObservedAt); err != nil {
		return errors.New("finish_step observed_at must be an actual RFC3339 observation time")
	}
	_, err = prepareFinalEvidence(ctx, f.job, f.runDir, Result{Status: "success", Text: text}, nil)
	return err
}

func (f *stepFinish) checkPublished(ctx context.Context, text, factID string) error {
	raw, err := readEvidenceFile(ctx, filepath.Join(f.runDir, "session.json"), 32<<20)
	if err != nil {
		return fmt.Errorf("finish_step fact_id requires a saved successful publication: %w", err)
	}
	var saved session
	if err := json.Unmarshal(raw, &saved); err != nil {
		return err
	}
	identity, err := identityFor(f.job, f.runDir)
	if err != nil {
		return err
	}
	// Validate the existing immutable session, then apply the proposed result
	// only to this in-memory validation view. No publication or terminal state
	// is written here. The normal outer graph repeats this exact check after
	// the accepted handoff has been saved as the real session result.
	if err := saved.validate(identity); err != nil {
		return err
	}
	result := Result{Type: "result", Status: "success", Text: text}
	saved.Result = &result
	validation, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	_, err = graphPublishedFactEvidence(ctx, f.job, f.runDir, identity, validation, result, factID)
	if err != nil {
		return fmt.Errorf("finish_step fact_id must name a valid evidence-backed fact published by this Step and run: %w", err)
	}
	return nil
}

func (f *stepFinish) recover(ctx context.Context) error {
	raw, err := readEvidenceFile(ctx, filepath.Join(f.runDir, "finish-step.json"), MaxGraphRPCBytes*4)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var receipt stepFinishReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return err
	}
	identity, err := identityFor(f.job, f.runDir)
	if err != nil {
		return err
	}
	if receipt.Identity != identity {
		return errors.New("finish_step receipt belongs to a different immutable job")
	}
	if err := f.check(ctx, receipt.Text); err != nil {
		return err
	}
	f.text = receipt.Text
	return nil
}

func (f *stepFinish) tool() agent.Tool {
	return agent.Tool{Definition: agent.Definition{
		Name:        "finish_step",
		Description: "Explicitly finish this assigned Step after its checks and deliverables are verified. Choose exactly one: fact_id for an evidence-backed fact already published by this Step in this run, or fact for one new observation. If graph_action already returned a fact ID, use {fact_id:that ID}; do not restate it as another fact. The runtime validates the saved publication receipt and original evidence without a new graph read. A publication in the current tool batch must settle first. For a new fact, the runtime freezes exact bytes and passes it through existing Dispatcher acceptance/publication checks, so no preceding graph_action fact or completion echo is needed. This ends the Worker without another model confirmation. A successful command alone does not establish the claim or satisfy the task: inspect returned output and required artifacts first. run_graph verified output previews and evidence selections can support a new fact without rereading unchanged files. Shared curation and project completion remain separate. Omit run_id and excerpt; runtime supplies them.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"fact_id":{"type":"string","minLength":1,"maxLength":256,"description":"Use the ID from this run's successful graph_action fact receipt instead of republishing it."},"fact":{"type":"object","properties":{"description":{"type":"string","minLength":1},"scope":{"type":"string","minLength":1},"observed_at":{"type":"string","format":"date-time"},"evidence":{"type":"array","minItems":1,"maxItems":32,"items":{"type":"object","description":"` + evidenceSelectionDescription + `","properties":{"path":{"type":"string","minLength":1},"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}}},"required":["description","scope","observed_at","evidence"],"additionalProperties":false}},"oneOf":[{"required":["fact_id"]},{"required":["fact"]}],"additionalProperties":false}`),
	}, Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
		// Keep the original selection as the terminal input. The normal finish
		// path reuses its prepared evidence manifest instead of preparing a
		// different request containing already-rewritten snapshot paths.
		if err := agent.ValidateArguments(f.tool().Schema, raw); err != nil {
			return "", err
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(raw, &data); err != nil {
			return "", err
		}
		encoded, err := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": data})
		if err != nil {
			return "", err
		}
		text := string(encoded)
		if err := f.check(ctx, text); err != nil {
			return "", err
		}
		identity, err := identityFor(f.job, f.runDir)
		if err != nil {
			return "", err
		}
		receipt, err := json.Marshal(stepFinishReceipt{Identity: identity, Text: text})
		if err != nil {
			return "", err
		}
		if err := retainEvidenceFile(ctx, filepath.Join(f.runDir, "finish-step.json"), receipt); err != nil {
			return "", err
		}
		f.text = text
		return `{"status":"handoff_ready","evidence_frozen":true,"step_acceptance":"pending_dispatcher"}`, nil
	}}
}
