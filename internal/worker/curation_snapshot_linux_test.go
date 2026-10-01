//go:build linux

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
			return GraphPage(frozen, request)
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
	if err := ConfigureRuntimeTools(j, &o); err != nil {
		t.Fatal(err)
	}
	if len(o.Tools) != 3 || o.Tools[2].Name != "read_evidence" {
		t.Fatal("snapshot added redundant curator tools")
	}
	view, err := jobContextView(j)
	if err != nil || len(view) > board.DefaultContextViewBytes {
		t.Fatal("unbounded curator prompt")
	}
	read := func(request GraphRequest) string {
		input, _ := json.Marshal(request)
		raw, err := o.Tools[0].Execute(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	var page graphPage
	if json.Unmarshal([]byte(read(GraphRequest{Section: "candidates", IDs: []string{"candidate_a"}})), &page) != nil || len(page.Items) != 1 {
		t.Fatal("snapshot candidate missing")
	}
	var ref graphRecordReference
	if json.Unmarshal(page.Items[0], &ref) != nil || !ref.RecordOmitted {
		t.Fatal("large record did not offer continuation")
	}
	content, offset := "", 0
	for {
		var part graphContentPage
		if json.Unmarshal([]byte(read(GraphRequest{Section: "candidates", IDs: []string{"candidate_a"}, ByteOffset: &offset, RecordVersion: ref.RecordVersion})), &part) != nil {
			t.Fatal("invalid continuation")
		}
		content += part.Content
		if part.NextByteOffset == nil {
			break
		}
		offset = *part.NextByteOffset
	}
	var candidate board.Candidate
	if json.Unmarshal([]byte(content), &candidate) != nil || candidate.Reason != frozen.Candidates[0].Reason {
		t.Fatal("snapshot changed original bytes")
	}
	if _, err := o.Tools[1].Execute(context.Background(), json.RawMessage(`{"op":"curate","idempotency_key":"batch","payload":{"groups":[]}}`)); err != nil {
		t.Fatal(err)
	}
	if reads < 3 || writes != 1 || *o.GraphVersion != j.InputSnapshot.StateVersion {
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
	if err := ConfigureRuntimeTools(j, &o); err != nil {
		t.Fatal(err)
	}
	read := o.Tools[0]
	if strings.Contains(read.Description, "refresh overview") || !strings.Contains(read.Description, "immutable curation snapshot") {
		t.Fatal("curator read protocol must not suggest replacing its immutable input")
	}
	call := func(request GraphRequest) (string, error) {
		raw, _ := json.Marshal(request)
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
		t.Fatalf("missing oversized candidate reference: %s %v", raw, err)
	}
	var reference graphRecordReference
	if json.Unmarshal(page.Items[0], &reference) != nil || !reference.RecordOmitted {
		t.Fatalf("oversized candidate bypassed record paging: %s", raw)
	}
	content, offset := "", 0
	for {
		raw, err := call(GraphRequest{Section: "candidates", IDs: []string{"candidate_a"}, ByteOffset: &offset, ExpectedVersion: version, RecordVersion: reference.RecordVersion})
		var fragment graphContentPage
		if err != nil || json.Unmarshal([]byte(raw), &fragment) != nil {
			t.Fatalf("snapshot continuation failed: %s %v", raw, err)
		}
		content += fragment.Content
		if fragment.NextByteOffset == nil {
			break
		}
		offset = *fragment.NextByteOffset
	}
	var candidate board.CandidateView
	want := j.State.CandidateView(j.State.Candidates[0])
	if json.Unmarshal([]byte(content), &candidate) != nil || candidate.Reason != j.State.Candidates[0].Reason || candidate.GroupKey != want.GroupKey || candidate.SupportValid == nil || *candidate.SupportValid != *want.SupportValid {
		t.Fatal("snapshot paging changed original candidate bytes")
	}
	if _, err := call(GraphRequest{Section: "candidates", ExpectedVersion: strings.Repeat("f", 64)}); err == nil || !strings.Contains(err.Error(), "state_changed") {
		t.Fatalf("explicit wrong snapshot version accepted: %v", err)
	}
	if rpcs != 0 || *o.GraphVersion != version {
		t.Fatalf("reads used RPC or replaced immutable input: %d %s", rpcs, *o.GraphVersion)
	}
	if _, err := o.Tools[1].Execute(context.Background(), json.RawMessage(`{"op":"curate","idempotency_key":"batch","payload":{"groups":[]}}`)); err == nil || !strings.Contains(err.Error(), "state_changed") || rpcs != 1 {
		t.Fatalf("local reads bypassed authoritative commit CAS: RPCs=%d err=%v", rpcs, err)
	}
}

func TestCuratorPromptAndCandidateReadsShareGroupingAndConfidenceHints(t *testing.T) {
	j := curationJob(t)
	j.State.FactRecords = []board.FactRecord{{ID: "fact_a", Status: "valid"}}
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
	for _, rule := range []string{"equal group_key", "not a graph ID", "same-status producer candidate with support_valid=true", "Use resolution and review_fact_ids only for an existing dispute"} {
		if !strings.Contains(o.Tools[1].Description, rule) {
			t.Fatalf("curator contract does not explain %q", rule)
		}
	}
	if !strings.Contains(string(o.Tools[1].Schema), "read-only grouping hint") || !strings.Contains(string(o.Tools[1].Schema), "omit group_key") {
		t.Fatal("curation group fields do not distinguish input hints from writable fields")
	}
	if err := agent.ValidateArguments(o.Tools[1].Schema, json.RawMessage(`{"op":"curate","idempotency_key":"group","payload":{"groups":[{"candidate_ids":["candidate_a"],"status":"candidate","reason":"Uncertain","group_key":"read-only"}]}}`)); err == nil || !strings.Contains(err.Error(), "group_key") {
		t.Fatalf("read-only group hint was accepted as a write: %v", err)
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
