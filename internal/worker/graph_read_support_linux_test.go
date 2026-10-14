//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"pwnmesh/internal/protocol"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

// Produce the aggregate through the public action boundary: no individual
// publication exceeds the bridge frame, but their shared Finding exceeds 1 MiB.
func largeFindingReadFixture(t *testing.T) board.State {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "finding.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var state board.State
	err = store.Do(context.Background(), func(tx *board.Tx) error {
		graph := board.Graph{Project: board.Project{ID: "p", Title: "Finding support", Status: "active", CreatedAt: tx.Now, OrchestrationVersion: 1}, Facts: []board.Fact{{ID: "origin", Description: "input"}, {ID: "goal", Description: "Review original observations"}}}
		for i := range 2 {
			graph.Intents = append(graph.Intents, board.Intent{ID: fmt.Sprintf("i%d", i), From: []string{"origin"}, Description: "Collect observations", Creator: "planner", Worker: board.Ptr(fmt.Sprintf("worker@p%d", i)), Heartbeat: board.Ptr(tx.Now), CreatedAt: tx.Now})
		}
		if err := tx.Save(graph); err != nil {
			return err
		}
		factPayload, _ := json.Marshal(map[string]any{"description": "Observed source", "scope": "fixture", "observed_at": tx.Now, "evidence": []board.EvidenceRef{{RunID: "worker@p0", Path: "/workspace/retained/source.raw", Excerpt: "Original source"}}})
		fact, err := tx.StateAction("p", board.ExecutionFence{Run: "worker@p0", Lease: "explore", Intent: "i0"}, board.StateAction{Op: "fact", IdempotencyKey: "source", Payload: factPayload})
		if err != nil {
			return err
		}
		ids := []string{}
		for n := range 11 {
			fence := board.ExecutionFence{Run: fmt.Sprintf("worker@p%d", n%2), Lease: "explore", Intent: fmt.Sprintf("i%d", n%2)}
			refs := []board.EvidenceRef{}
			for i := range 12 {
				refs = append(refs, board.EvidenceRef{RunID: fence.Run, Path: fmt.Sprintf("/workspace/retained/%d-%d.raw", n, i), Excerpt: strings.Repeat("x", 8192)})
			}
			payload, _ := json.Marshal(map[string]any{"claim": "Aggregate metadata remains readable", "scope": "fixture", "status": "candidate", "reason": "Retain original observations", "sources": []string{fact.ID}, "evidence": refs})
			action := board.StateAction{Op: "candidate", IdempotencyKey: fmt.Sprintf("candidate%d", n), Payload: payload}
			wire, _ := json.Marshal(GraphRequestEvent{Type: "graph_request", Request: GraphRequest{RequestID: strings.Repeat("a", 32), Op: "graph_action", Action: action}})
			if len(wire) > MaxGraphRPCBytes {
				return fmt.Errorf("publication exceeds the bridge frame: %d", len(wire))
			}
			result, err := tx.StateAction("p", fence, action)
			if err != nil {
				return err
			}
			ids = append(ids, result.ID)
		}
		e := board.Execution{ProjectID: "p", ID: "merge", Namespace: "test", Backend: "curator", Kind: "curate", Lease: "curator@merge"}
		if _, err := tx.ClaimCurator("p", e.Lease, "reconcile"); err != nil {
			return err
		}
		input, err := tx.State("p")
		if err != nil {
			return err
		}
		ref, err := tx.FreezeInput(input)
		if err != nil {
			return err
		}
		view, err := board.CurationContextView(input, board.DefaultContextViewBytes)
		if err != nil {
			return err
		}
		e.Job, _ = json.Marshal(map[string]any{"kind": "curate", "run_id": e.ID, "graph": board.Graph{Project: input.Graph.Project}, "input_snapshot": ref, "input_view": view, "graph_rpc": true, "result_contract_version": 2})
		e.RetryKey = board.CurationRetryKey(input)
		if err := tx.RegisterExecution(e); err != nil {
			return err
		}
		payload, _ := json.Marshal(board.CuratePayload{ThroughRevision: input.Revision, Groups: []board.CurateGroup{{CandidateIDs: ids, Status: "candidate", Reason: "Retain the complete producer group"}}})
		if _, err := tx.StateAction("p", e.Fence(), board.StateAction{Op: "curate", IdempotencyKey: "merge", ExpectedVersion: board.DecisionStateVersion(input), Payload: payload}); err != nil {
			return err
		}
		state, err = tx.State("p")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := board.ValidateContextCapacity(state); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state.Findings[0])
	if len(raw) <= maxModelGraphReadBytes || len(state.Findings[0].Evidence) != 132 {
		t.Fatal("fixture must produce an admitted aggregate over the model read bound")
	}
	return state
}

func TestModelGraphReadLargeFindingKeepsMetadataAndPagedSupport(t *testing.T) {
	state := largeFindingReadFixture(t)
	finding := state.Findings[0]
	request := modelReadFixture(t, state, nil)
	raw, err := modelGraphRead(context.Background(), GraphRequest{Op: "read_graph", Section: "findings", IDs: []string{finding.ID}, Limit: 1}, request)
	var page graphPage
	if err != nil || json.Unmarshal([]byte(raw), &page) != nil || len(page.Items) != 1 {
		t.Fatalf("legal aggregate lost its metadata: %v", err)
	}
	var metadata struct {
		board.Finding
		EvidenceOmitted bool `json:"evidence_omitted"`
		EvidenceCount   int  `json:"evidence_count"`
		SourcesOmitted  bool `json:"sources_omitted"`
		SourcesCount    int  `json:"sources_count"`
	}
	if err := json.Unmarshal(page.Items[0], &metadata); err != nil || !metadata.EvidenceOmitted || metadata.EvidenceCount != len(finding.Evidence) || !metadata.SourcesOmitted || metadata.SourcesCount != len(finding.Sources) {
		t.Fatal("large support omission is not explicit")
	}
	metadata.Evidence, finding.Evidence = nil, nil
	metadata.Sources, finding.Sources = nil, nil
	if !reflect.DeepEqual(metadata.Finding, finding) {
		t.Fatal("support paging changed finding metadata")
	}
	if strings.Contains(raw, "record_version") || strings.Contains(raw, "record_bytes") || strings.Contains(raw, "byte_offset") {
		t.Fatal("metadata fallback exposed transport continuation machinery")
	}
	var refs []board.EvidenceRef
	for offset := 0; ; {
		raw, err := modelGraphRead(context.Background(), GraphRequest{Op: "read_graph", Section: "evidence", IDs: []string{finding.ID}, Offset: offset, Limit: 50}, request)
		var support struct {
			Items []board.EvidenceRef `json:"items"`
			Next  *int                `json:"next_offset"`
		}
		if err != nil || json.Unmarshal([]byte(raw), &support) != nil || len(raw) > maxModelGraphReadBytes {
			t.Fatalf("support continuation failed: %v", err)
		}
		refs = append(refs, support.Items...)
		if support.Next == nil {
			break
		}
		offset = *support.Next
	}
	if !reflect.DeepEqual(refs, state.Findings[0].Evidence) {
		t.Fatal("support pagination lost or changed original evidence")
	}
	sources, err := modelGraphRead(context.Background(), GraphRequest{Op: "read_graph", Section: "sources", IDs: []string{finding.ID}}, request)
	var sourcePage struct {
		Items []string `json:"items"`
	}
	if err != nil || json.Unmarshal([]byte(sources), &sourcePage) != nil || !reflect.DeepEqual(sourcePage.Items, state.Findings[0].Sources) {
		t.Fatalf("source support became unavailable: %v", err)
	}

	t.Run("bounded_listing", func(t *testing.T) {
		listed := state
		last := finding
		last.ID = "later-finding"
		listed.Findings = append(append([]board.Finding{}, state.Findings...), last)
		request := modelReadFixture(t, listed, nil)
		first, err := modelGraphRead(context.Background(), GraphRequest{Op: "read_graph", Section: "findings", Limit: 1}, request)
		var page graphPage
		if err != nil || json.Unmarshal([]byte(first), &page) != nil || len(page.Items) != 1 || page.NextOffset == nil || *page.NextOffset != 1 || len(first) > maxModelGraphReadBytes {
			t.Fatalf("aggregate blocked bounded listing: %v", err)
		}
		next, err := modelGraphRead(context.Background(), GraphRequest{Op: "read_graph", Section: "findings", Offset: *page.NextOffset, Limit: 1}, request)
		page = graphPage{}
		if err != nil || json.Unmarshal([]byte(next), &page) != nil || len(page.Items) != 1 || page.NextOffset != nil || !strings.Contains(string(page.Items[0]), last.ID) {
			t.Fatalf("aggregate hid following records: %v", err)
		}
	})

	t.Run("decision_refresh", func(t *testing.T) {
		dir := t.TempDir()
		job := Job{Kind: "reason", GraphRPC: true, Decision: &board.DecisionContext{Version: 2, StateVersion: board.DecisionStateVersion(state)}}
		options := Options{RunDir: dir, Output: &draftTestBridge{dir: dir, handle: func(r GraphRequest) (any, error) { return protocol.GraphPage(state, r) }}}
		optionsRuntime := configureRoleTools(t, job, &options)
		optionsRuntime.decision.invalidate()
		read := options.Tools[0]
		if _, err := read.Execute(context.Background(), json.RawMessage(`{"section":"overview"}`)); err != nil {
			t.Fatal(err)
		}
		partial, err := read.Execute(context.Background(), json.RawMessage(`{"section":"findings"}`))
		if err != nil {
			t.Fatal(err)
		}
		loop := &agent.Loop{}
		optionsRuntime.decision.beforeRequest(loop)
		if !optionsRuntime.decision.reread || optionsRuntime.decision.detailRead || len(optionsRuntime.decision.refreshData) != 2 || !strings.Contains(optionsRuntime.decision.refreshData[1], partial) {
			t.Fatal("metadata omission authorized a stale decision or lost the partial observation")
		}
		input, _ := json.Marshal(map[string]any{"section": "evidence", "ids": []string{finding.ID}, "limit": 1})
		if _, err := read.Execute(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		if !optionsRuntime.decision.reread {
			t.Fatal("support tool call authorized replanning before the next model request")
		}
		optionsRuntime.decision.beforeRequest(loop)
		if optionsRuntime.decision.reread || len(loop.ContextData) != 3 || !strings.Contains(loop.ContextData[1], partial) {
			t.Fatal("actual support and retained metadata did not refresh the decision")
		}
	})
}
