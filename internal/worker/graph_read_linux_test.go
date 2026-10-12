//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board"
)

func modelReadFixture(t *testing.T, state board.State, inspect func(GraphRequest)) func(context.Context, GraphRequest) (string, error) {
	t.Helper()
	return func(_ context.Context, r GraphRequest) (string, error) {
		if inspect != nil {
			inspect(r)
		}
		page, err := GraphPage(state, r)
		if err != nil {
			return "", err
		}
		raw, err := json.Marshal(page)
		return string(raw), err
	}
}

func TestModelGraphReadAssemblesPagesAndOriginalSupport(t *testing.T) {
	state := board.State{}
	for i := 0; i < 5; i++ {
		state.FactRecords = append(state.FactRecords, board.FactRecord{ID: fmt.Sprintf("f%d", i), Description: strings.Repeat("x", 40<<10)})
	}
	for i := 0; i < 16; i++ {
		state.FactRecords[2].Evidence = append(state.FactRecords[2].Evidence, board.EvidenceRef{Path: fmt.Sprintf("/workspace/evidence-%d", i), Excerpt: strings.Repeat("λ<\n", 1500)})
	}
	ids := []string{"f0", "f1", "f2", "f3", "f4"}
	calls, fragments := 0, 0
	request := modelReadFixture(t, state, func(r GraphRequest) {
		calls++
		if r.Op != "read_snapshot" || !reflect.DeepEqual(r.IDs, ids) || !r.evidenceLookup {
			t.Fatalf("logical read changed its capability or selection: %+v", r)
		}
		if calls > 1 && r.ExpectedVersion != board.DecisionStateVersion(state) {
			t.Fatal("continuation lost the original state fence")
		}
		if r.ByteOffset != nil {
			fragments++
		}
	})
	raw, err := modelGraphRead(context.Background(), GraphRequest{Op: "read_snapshot", Section: "facts", IDs: ids, Limit: 5}, request)
	var result struct {
		Items []board.FactRecord `json:"items"`
		Next  *int               `json:"next_offset"`
	}
	if err != nil || json.Unmarshal([]byte(raw), &result) != nil || !reflect.DeepEqual(result.Items, state.FactRecords) || result.Next != nil || calls < 3 || fragments < 2 {
		t.Fatalf("read did not recover exact selected records/support: err=%v calls=%d fragments=%d items=%d", err, calls, fragments, len(result.Items))
	}
	if strings.Contains(raw, "record_omitted") || strings.Contains(raw, "evidence_omitted") || strings.Contains(raw, "next_byte_offset") {
		t.Fatal("transport reconstruction leaked into the model result")
	}
}

func TestModelGraphReadBoundsOutputAtWholeRecords(t *testing.T) {
	state := board.State{}
	for i := 0; i < 3; i++ {
		state.FactRecords = append(state.FactRecords, board.FactRecord{ID: fmt.Sprintf("f%d", i), Description: strings.Repeat("x", 400<<10)})
	}
	request := modelReadFixture(t, state, nil)
	raw, err := modelGraphRead(context.Background(), GraphRequest{Op: "read_graph", Section: "facts", Limit: 3}, request)
	var result graphPage
	if err != nil || json.Unmarshal([]byte(raw), &result) != nil || len(raw) > maxModelGraphReadBytes || len(result.Items) != 2 || result.NextOffset == nil || *result.NextOffset != 2 {
		t.Fatalf("logical page was truncated or lost its continuation: bytes=%d items=%d err=%v", len(raw), len(result.Items), err)
	}
	for i, raw := range result.Items {
		var fact board.FactRecord
		if json.Unmarshal(raw, &fact) != nil || !reflect.DeepEqual(fact, state.FactRecords[i]) {
			t.Fatal("output bound truncated an individual record")
		}
	}
	last, err := modelGraphRead(context.Background(), GraphRequest{Op: "read_graph", Section: "facts", Offset: *result.NextOffset, Limit: 3}, request)
	result = graphPage{}
	if err != nil || json.Unmarshal([]byte(last), &result) != nil || len(result.Items) != 1 || result.NextOffset != nil {
		t.Fatalf("remaining record became unavailable: %v", err)
	}
}

func TestModelGraphReadRejectsChangedOrMalformedFragments(t *testing.T) {
	state := board.State{Graph: board.Graph{Facts: []board.Fact{{ID: "origin", Description: strings.Repeat("λ", MaxGraphRPCBytes)}}}}
	for _, mode := range []string{"state", "hash", "offset", "content", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			base := modelReadFixture(t, state, nil)
			request := func(ctx context.Context, r GraphRequest) (string, error) {
				raw, err := base(ctx, r)
				if err != nil || r.ByteOffset == nil {
					return raw, err
				}
				var part graphContentPage
				if err := json.Unmarshal([]byte(raw), &part); err != nil {
					return "", err
				}
				switch mode {
				case "state":
					part.StateVersion = strings.Repeat("b", 64)
				case "hash":
					part.RecordVersion = strings.Repeat("b", 64)
				case "offset":
					part.ByteOffset++
				case "content":
					part.Content = strings.Replace(part.Content, "λ", "μ", 1)
				case "oversize":
					part.TotalBytes = maxModelGraphReadBytes + 1
				}
				encoded, err := json.Marshal(part)
				return string(encoded), err
			}
			if raw, err := modelGraphRead(context.Background(), GraphRequest{Op: "read_graph", Section: "overview"}, request); err == nil || raw != "" {
				t.Fatalf("unverified fragments reached the model: mode=%s err=%v bytes=%d", mode, err, len(raw))
			}
		})
	}
}

func TestModelGraphReadRejectsStaleCollectionContinuation(t *testing.T) {
	state := board.State{FactRecords: []board.FactRecord{{ID: "a", Description: strings.Repeat("a", 70<<10)}, {ID: "b", Description: strings.Repeat("b", 70<<10)}}}
	calls := 0
	request := func(_ context.Context, r GraphRequest) (string, error) {
		calls++
		if calls == 2 {
			state.FactRecords[0].Description = "new evidence"
		}
		return modelReadFixture(t, state, nil)(context.Background(), r)
	}
	if raw, err := modelGraphRead(context.Background(), GraphRequest{Op: "read_graph", Section: "facts", Limit: 2}, request); err == nil || !strings.Contains(err.Error(), "state_changed") || raw != "" || calls != 2 {
		t.Fatalf("mixed graph versions reached the model: calls=%d err=%v bytes=%d", calls, err, len(raw))
	}
}

func TestModelGraphReadRejectsMalformedSupportProjection(t *testing.T) {
	state := board.State{Findings: []board.Finding{{ID: "finding", Claim: "Retained claim", Evidence: []board.EvidenceRef{{Path: "/workspace/a", Excerpt: strings.Repeat("x", 200<<10)}}}}}
	for _, mode := range []string{"shape", "size", "digest", "count", "present", "hash"} {
		t.Run(mode, func(t *testing.T) {
			base := modelReadFixture(t, state, nil)
			request := func(ctx context.Context, r GraphRequest) (string, error) {
				raw, err := base(ctx, r)
				if err != nil || r.ByteOffset != nil {
					return raw, err
				}
				var page graphPage
				_ = json.Unmarshal([]byte(raw), &page)
				var item map[string]json.RawMessage
				_ = json.Unmarshal(page.Items[0], &item)
				switch mode {
				case "shape":
					page.Items[0] = json.RawMessage(`null`)
				case "size":
					item["record_bytes"] = json.RawMessage(`-1`)
				case "digest":
					item["record_version"], _ = json.Marshal(strings.Repeat("z", 64))
				case "count":
					item["evidence_count"] = json.RawMessage(`0`)
				case "present":
					item["evidence"] = json.RawMessage(`[]`)
				case "hash":
					item["record_version"], _ = json.Marshal(strings.Repeat("b", 64))
				}
				if mode != "shape" {
					page.Items[0], _ = json.Marshal(item)
				}
				encoded, err := json.Marshal(page)
				return string(encoded), err
			}
			if raw, err := modelGraphRead(context.Background(), GraphRequest{Op: "read_graph", Section: "findings"}, request); err == nil || raw != "" {
				t.Fatalf("malformed metadata or changed bound content was accepted: %s %v", mode, err)
			}
		})
	}
}

func TestModelGraphReadSupportFallbackIncludesPageEnvelope(t *testing.T) {
	finding := board.Finding{ID: "finding", Claim: "Keep the metadata", Evidence: []board.EvidenceRef{{Path: "/workspace/a", Excerpt: "x"}}}
	raw, _ := json.Marshal(finding)
	finding.Evidence[0].Excerpt = strings.Repeat("x", maxModelGraphReadBytes-len(raw)-32)
	state := board.State{Findings: []board.Finding{finding}}
	raw, _ = json.Marshal(finding)
	if len(raw) >= maxModelGraphReadBytes {
		t.Fatal("fixture record must fit without its page envelope")
	}
	result, err := modelGraphRead(context.Background(), GraphRequest{Op: "read_graph", Section: "findings", Limit: 1}, modelReadFixture(t, state, nil))
	if err != nil || len(result) > maxModelGraphReadBytes || !strings.Contains(result, `"evidence_omitted":true`) || !strings.Contains(result, finding.Claim) {
		t.Fatalf("page envelope made otherwise readable metadata inaccessible: %v", err)
	}
}
