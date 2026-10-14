//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"path/filepath"
	eventlog "pwnmesh/internal/worker/internal/journal"
	"reflect"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

// A valid request near the bridge limit can produce a compacted response.
// Publication must still be usable by finish_step without another graph read.
func TestFinishStepUsesCompactedPublicationEvidence(t *testing.T) {
	for _, variant := range []string{"selected_lines", "legacy_shorter_excerpt"} {
		t.Run(variant, func(t *testing.T) {
			testCompactedPublicationEvidence(t, variant == "legacy_shorter_excerpt")
		})
	}
}

func testCompactedPublicationEvidence(t *testing.T, legacyExcerpt bool) {
	j, dir := graphWrapperJob(t), t.TempDir()
	j.ResultContractVersion, j.GraphRPC = 2, true
	ctx := context.Background()
	var action board.StateAction
	var fact board.FactRecord
	var receiptRaw []byte
	for size := 7600; size <= 8190; size++ {
		// Keep a legacy shorter excerpt inside the selected line: reconstructing
		// the entire line would certify different bytes than the publication.
		text := strings.Repeat("x", size)
		ref := board.EvidenceRef{RunID: j.RunID, Path: filepath.Join(dir, "evidence", strings.Repeat("a", 64)+".raw"), Excerpt: text, StartLine: 1, EndLine: 1}
		refs := make([]board.EvidenceRef, 16)
		for i := range refs {
			refs[i] = ref
		}
		payload, _ := json.Marshal(map[string]any{"description": "Bound observation", "scope": "local fixture", "observed_at": "2026-10-10T00:00:00Z", "evidence": refs})
		action = board.StateAction{Op: "fact", IdempotencyKey: "published", Payload: payload}
		fact = board.FactRecord{ID: "f_published", Description: "Bound observation", Scope: "local fixture", ObservedAt: "2026-10-10T00:00:00Z", Status: "valid", RunID: "worker@" + j.RunID, SourceStepID: j.Intent.ID, Evidence: refs}
		factRaw, _ := json.Marshal(fact)
		receiptRaw, _ = json.Marshal(board.StateActionResult{Op: "fact", ID: fact.ID, Revision: 1, StateVersion: strings.Repeat("b", 64), Result: factRaw})
		if len(receiptRaw) > maxGraphPageBytes+128 {
			break
		}
	}
	original := fact.Evidence[0].Excerpt
	if legacyExcerpt {
		original = "prefix " + original + " suffix\n"
	}
	retained, err := freezeEvidenceBytes(ctx, j.RunID, dir, []byte(original))
	if err != nil {
		t.Fatal(err)
	}
	for i := range fact.Evidence {
		fact.Evidence[i].Path = retained.Path
	}
	action.Payload, _ = json.Marshal(map[string]any{"description": fact.Description, "scope": fact.Scope, "observed_at": fact.ObservedAt, "evidence": fact.Evidence})
	factRaw, _ := json.Marshal(fact)
	receiptRaw, _ = json.Marshal(board.StateActionResult{Op: "fact", ID: fact.ID, Revision: 1, StateVersion: strings.Repeat("b", 64), Result: factRaw})
	requestRaw, _ := json.Marshal(GraphRequestEvent{Type: "graph_request", Request: GraphRequest{RequestID: strings.Repeat("a", 32), Op: "graph_action", Action: action}})
	if len(requestRaw) > MaxGraphRPCBytes || len(receiptRaw) <= maxGraphPageBytes {
		t.Fatalf("fixture misses valid request/compacted response boundary: request=%d response=%d", len(requestRaw), len(receiptRaw))
	}
	compact, err := CompactGraphActionResult(receiptRaw)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(GraphResponse{RequestID: strings.Repeat("a", 32), Result: compact})
	if len(wire) > MaxGraphRPCBytes {
		t.Fatal("compacted publication exceeds the transport limit")
	}
	if !strings.Contains(string(compact), `"evidence_excerpts_omitted":true`) || !strings.Contains(string(compact), `"evidence_excerpt_sha256"`) {
		t.Fatal("compacted publication hides the omitted excerpts or mislabels their hashes")
	}
	originalAction := action
	if !legacyExcerpt {
		// Current model calls select paths/lines; run identity and excerpts are
		// added by prepareEvidence only after the tool call has been recorded.
		selections := make([]map[string]any, len(fact.Evidence))
		for i, ref := range fact.Evidence {
			selections[i] = map[string]any{"path": ref.Path, "start_line": ref.StartLine, "end_line": ref.EndLine}
		}
		originalAction.Payload, _ = json.Marshal(map[string]any{"description": fact.Description, "scope": fact.Scope, "observed_at": fact.ObservedAt, "evidence": selections})
	}
	actionRaw, _ := json.Marshal(originalAction)
	content, _ := json.Marshal(string(compact))
	history := []agent.Message{
		{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "publish", Name: "graph_action", Input: actionRaw}}},
		{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "publish", Content: content}}},
	}
	identity, err := identityFor(j, dir)
	if err != nil {
		t.Fatal(err)
	}
	result := Result{Status: "success", Text: `{"accepted":true,"outcome":"completed","data":{"fact_id":"f_published"}}`}
	saved := session{SchemaVersion: sessionSchemaVersion, Identity: identity, RunID: j.RunID, Kind: j.Kind, StartedAt: time.Now(), Result: &result, History: history}
	journal, err := eventlog.Open(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	for _, message := range history {
		if err := journal.Append(agent.Event{Type: "message_end", Message: &message}); err != nil {
			t.Fatal(err)
		}
	}
	if err := saved.save(dir, journal); err != nil {
		t.Fatal(err)
	}
	for _, compactHistory := range []bool{false, true} {
		if compactHistory {
			saved.History = []agent.Message{agent.Text("user", "Publication is retained in the committed journal.")}
			if err := saved.save(dir, journal); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := readEvidenceFile(ctx, filepath.Join(dir, "session.json"), 32<<20)
		if err != nil {
			t.Fatal(err)
		}
		got, err := graphPublishedFactEvidence(ctx, j, dir, identity, raw, result, fact.ID)
		if err != nil || !reflect.DeepEqual(got, fact.Evidence) {
			t.Fatalf("successful compact publication lost exact evidence (history compacted=%t): %v", compactHistory, err)
		}
		if err := (&stepFinish{job: j, runDir: dir}).check(ctx, result.Text); err != nil {
			t.Fatalf("successful compact publication cannot finish: %v", err)
		}
	}
	for _, variant := range []string{"changed_digest", "missing_digest", "missing_omission_marker", "changed_selection"} {
		t.Run(variant, func(t *testing.T) {
			var receipt board.StateActionResult
			if err := json.Unmarshal(compact, &receipt); err != nil {
				t.Fatal(err)
			}
			var publication compactFactPublication
			if err := json.Unmarshal(receipt.Result, &publication); err != nil {
				t.Fatal(err)
			}
			changedAction := originalAction
			switch variant {
			case "changed_digest":
				publication.EvidenceExcerptSHA256[0] = strings.Repeat("0", 64)
			case "missing_digest":
				publication.EvidenceExcerptSHA256 = publication.EvidenceExcerptSHA256[1:]
			case "missing_omission_marker":
				publication.EvidenceExcerptsOmitted = false
			case "changed_selection":
				// All bytes remain valid original evidence, but this is not the
				// shorter excerpt acknowledged by the successful publication.
				refs := append([]board.EvidenceRef{}, fact.Evidence...)
				refs[0].Excerpt = refs[0].Excerpt[1:]
				changedAction.Payload, _ = json.Marshal(map[string]any{"evidence": refs})
			}
			receipt.Result, _ = json.Marshal(publication)
			encoded, _ := json.Marshal(receipt)
			content, _ := json.Marshal(string(encoded))
			input, _ := json.Marshal(changedAction)
			saved.History = []agent.Message{
				{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "publish", Name: "graph_action", Input: input}}},
				{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "publish", Content: content}}},
			}
			raw, _ := json.Marshal(saved)
			if _, err := graphPublishedFactEvidence(ctx, j, dir, identity, raw, result, fact.ID); err == nil {
				t.Fatal("changed compact publication evidence was accepted")
			}
		})
	}
}
