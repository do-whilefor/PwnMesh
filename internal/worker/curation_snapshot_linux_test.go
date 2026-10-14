//go:build linux

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"pwnmesh/internal/protocol"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func TestCuratorSnapshotReadsRetainedPagesAndBindsCommit(t *testing.T) {
	j, dir := curationJob(t), t.TempDir()
	j.State.Candidates[0].Reason = strings.Repeat("retained observation ", 14000)
	frozen := *j.State
	raw, _ := json.Marshal(frozen)
	j.InputSnapshot = &board.InputSnapshot{Version: 1, ID: fmt.Sprintf("%x", sha256.Sum256(raw)), ProjectID: j.Graph.Project.ID, Generation: j.Graph.Project.Generation, Revision: frozen.Revision, StateVersion: board.DecisionStateVersion(frozen)}
	var err error
	j.InputView, err = board.CurationContextView(frozen, board.DefaultContextViewBytes)
	if err != nil {
		t.Fatal(err)
	}
	j.State, j.Graph = nil, board.Graph{Project: j.Graph.Project}
	reads, writes := 0, 0
	bridge := &draftTestBridge{dir: dir, handle: func(request GraphRequest) (any, error) {
		switch request.Op {
		case "read_snapshot":
			reads++
			if request.ExpectedVersion != j.InputSnapshot.StateVersion {
				t.Fatal("snapshot read lost its version")
			}
			return protocol.GraphPage(frozen, request)
		case "graph_action":
			writes++
			var payload board.CuratePayload
			if json.Unmarshal(request.Action.Payload, &payload) != nil || payload.ThroughRevision != j.InputSnapshot.Revision || request.Action.ExpectedVersion != j.InputSnapshot.StateVersion {
				t.Fatal("curation commit lost its immutable boundary")
			}
			return board.StateActionResult{Committed: true, StateVersion: j.InputSnapshot.StateVersion}, nil
		default:
			return nil, fmt.Errorf("unexpected request %s", request.Op)
		}
	}}
	o := Options{RunDir: dir, Output: bridge}
	oRuntime := configureRoleTools(t, j, &o)
	if len(o.Tools) != 3 || o.Tools[2].Name != "read_evidence" {
		t.Fatal("snapshot added redundant curator tools")
	}
	view, err := jobContextView(j)
	if err != nil || len(view) > board.DefaultContextViewBytes {
		t.Fatal("unbounded curator prompt")
	}
	result, err := o.Tools[0].Execute(context.Background(), json.RawMessage(`{"section":"candidates","ids":["candidate_a"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var page graphPage
	if json.Unmarshal([]byte(result), &page) != nil || len(page.Items) != 1 {
		t.Fatal("snapshot candidate missing")
	}
	var candidate board.Candidate
	if json.Unmarshal(page.Items[0], &candidate) != nil || candidate.Reason != frozen.Candidates[0].Reason || strings.Contains(result, "record_omitted") {
		t.Fatal("runtime did not restore the original snapshot record")
	}
	if _, err := o.Tools[1].Execute(context.Background(), json.RawMessage(`{"op":"curate","payload":{"groups":[]}}`)); err != nil {
		t.Fatal(err)
	}
	if reads < 3 || writes != 1 || oRuntime.graph.Version != j.InputSnapshot.StateVersion {
		t.Fatal("snapshot read or commit was not bound")
	}
}

func TestCuratorReadsImmutableSnapshotLocallyAndKeepsCommitCAS(t *testing.T) {
	j, dir := curationJob(t), t.TempDir()
	j.State.Candidates[0].Reason = strings.Repeat("retained original bytes ", 12000)
	version := board.DecisionStateVersion(*j.State)
	rpcs := 0
	bridge := &draftTestBridge{dir: dir, handle: func(request GraphRequest) (any, error) {
		rpcs++
		if request.Op != "graph_action" || request.Action.ExpectedVersion != version {
			t.Fatalf("read escaped to live state or write lost snapshot CAS: %+v", request)
		}
		return nil, errors.New("state_changed: newer observations arrived")
	}}
	o := Options{RunDir: dir, Output: bridge}
	oRuntime := configureRoleTools(t, j, &o)
	read := o.Tools[0]
	if strings.Contains(read.Description, "refresh overview") || !strings.Contains(read.Description, "immutable curation snapshot") {
		t.Fatal("curator read protocol must not suggest replacing its immutable input")
	}
	call := func(request GraphRequest) (string, error) {
		raw, _ := json.Marshal(map[string]any{"section": request.Section, "ids": request.IDs, "limit": request.Limit})
		return read.Execute(context.Background(), raw)
	}
	for _, section := range []string{"overview", "facts", "steps", "disputes", "relations", "sources"} {
		request := GraphRequest{Section: section}
		if section == "sources" {
			request.IDs = []string{"candidate_a"}
		}
		if _, err := call(request); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := call(GraphRequest{Section: "candidates", IDs: []string{"candidate_a"}, Limit: 1})
	var page graphPage
	if err != nil || json.Unmarshal([]byte(raw), &page) != nil || len(page.Items) != 1 {
		t.Fatalf("missing assembled candidate: %v", err)
	}
	var candidate board.CandidateView
	want := j.State.CandidateView(j.State.Candidates[0])
	if json.Unmarshal(page.Items[0], &candidate) != nil || candidate.Reason != j.State.Candidates[0].Reason || candidate.GroupKey != want.GroupKey || candidate.SupportValid == nil || *candidate.SupportValid != *want.SupportValid {
		t.Fatal("snapshot paging changed original candidate bytes")
	}
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"section":"candidates","expected_version":"model-version"}`)); err == nil {
		t.Fatal("model supplied a transport-only snapshot version")
	}
	if rpcs != 0 || oRuntime.graph.Version != version {
		t.Fatalf("reads used RPC or replaced immutable input: %d %s", rpcs, oRuntime.graph.Version)
	}
	if _, err := o.Tools[1].Execute(context.Background(), json.RawMessage(`{"op":"curate","payload":{"groups":[]}}`)); err == nil || !strings.Contains(err.Error(), "state_changed") || rpcs != 1 {
		t.Fatalf("local reads bypassed authoritative commit CAS: RPCs=%d err=%v", rpcs, err)
	}
}

func TestCuratorPromptAndCandidateReadsShareGroupingAndConfidenceHints(t *testing.T) {
	j := curationJob(t)
	ref, err := freezeEvidenceBytes(context.Background(), "producer-run", t.TempDir(), []byte("GET /fixture without authentication\nHTTP/1.1 200 OK\nfixture response\n"))
	if err != nil {
		t.Fatal(err)
	}
	j.State.FactRecords = []board.FactRecord{{ID: "fact_a", Status: "valid", Description: "The fixture returned a successful response", Scope: "unauthenticated request", ObservedAt: "2026-09-22T10:00:00Z", RunID: ref.RunID, Evidence: []board.EvidenceRef{ref}}}
	j.State.Candidates[0].Revision = j.State.Revision
	j.State.Candidates[0].Status = "candidate" // Valid evidence alone does not raise producer confidence.
	// Revisiting a shared finding requires reconciliation; an ordinary note
	// intentionally does not occupy the curator's initial input anymore.
	j.State.Findings = []board.Finding{{ID: "shared", Claim: j.State.Candidates[0].Claim, Scope: j.State.Candidates[0].Scope, Status: "candidate"}}
	before, _ := json.Marshal(j)
	version := board.DecisionStateVersion(*j.State)
	raw, err := jobContextView(j)
	if err != nil {
		t.Fatal(err)
	}
	var initial struct {
		Candidates []board.CandidateView `json:"candidates"`
	}
	if json.Unmarshal(raw, &initial) != nil || len(initial.Candidates) != 1 {
		t.Fatal("curator input omitted its available candidate")
	}
	o := Options{RunDir: t.TempDir()}
	if err := ConfigureRuntimeTools(j, &o); err != nil {
		t.Fatal(err)
	}
	rawPage, err := o.Tools[0].Execute(context.Background(), json.RawMessage(`{"section":"candidates","ids":["candidate_a"]}`))
	var page graphPage
	var candidate board.CandidateView
	if err != nil || json.Unmarshal([]byte(rawPage), &page) != nil || len(page.Items) != 1 || json.Unmarshal(page.Items[0], &candidate) != nil {
		t.Fatalf("candidate hint page failed: %v", err)
	}
	if candidate.GroupKey == "" || candidate.GroupKey != initial.Candidates[0].GroupKey || candidate.SupportValid == nil || !*candidate.SupportValid || candidate.Status != "candidate" {
		t.Fatalf("prompt/read grouping diverged or candidate confidence was promoted: %+v", candidate)
	}
	contract := o.Tools[1].Description + "\n" + string(o.Tools[1].Schema)
	for _, rule := range []string{"group_key", "complete group", "support_valid=true", "Independent review evidence for an existing dispute only"} {
		if !strings.Contains(contract, rule) {
			t.Fatalf("curator contract does not explain %q", rule)
		}
	}
	if !strings.Contains(string(o.Tools[1].Schema), "exact group_key") || !strings.Contains(string(o.Tools[1].Schema), "remaining groups stay queued") {
		t.Fatal("curation selectors do not explain complete groups and bounded batches")
	}
	for _, selector := range []string{`"candidate_ids":["candidate_a"]`, `"group_key":"` + candidate.GroupKey + `"`} {
		if err := agent.ValidateArguments(o.Tools[1].Schema, json.RawMessage(`{"op":"curate","payload":{"groups":[{`+selector+`,"status":"candidate","reason":"Uncertain"}]}}`)); err != nil {
			t.Fatalf("complete group selector is unavailable to the model: %v", err)
		}
	}
	after, _ := json.Marshal(j)
	if string(before) != string(after) || page.StateVersion != version || board.DecisionStateVersion(*j.State) != version {
		t.Fatal("projection fields changed the immutable job or CAS boundary")
	}
}

func TestGraphReadFieldHintsKeepSupportSelectorsExplicit(t *testing.T) {
	j := curationJob(t)
	o := Options{RunDir: t.TempDir()}
	if err := ConfigureRuntimeTools(j, &o); err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct{ Description string }
	}
	if err := json.Unmarshal(o.Tools[0].Schema, &schema); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(schema.Properties["ids"].Description, "Exactly one owning record ID") || !strings.Contains(schema.Properties["section"].Description, "require exactly one ID") {
		t.Fatal("support selection requirements are absent from the input fields")
	}
	for _, section := range []string{"evidence", "sources"} {
		if _, err := o.Tools[0].Execute(context.Background(), json.RawMessage(`{"section":"`+section+`","offset":0,"limit":20}`)); err == nil || !strings.Contains(err.Error(), "exactly one record ID") {
			t.Fatalf("support page accepted an unbound collection listing: %s %v", section, err)
		}
	}
	for _, input := range []string{`{"section":"candidates","offset":0,"limit":20}`, `{"section":"sources","ids":["candidate_a"]}`, `{"section":"evidence","ids":["candidate_a"]}`} {
		if _, err := o.Tools[0].Execute(context.Background(), json.RawMessage(input)); err != nil {
			t.Fatalf("documented selector rejected: %s: %v", input, err)
		}
	}
}
