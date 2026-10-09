package server

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board"
)

func TestSchedulingChecksAreOptionalAndPaged(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		g, err := tx.Load(f.project)
		if err != nil {
			return err
		}
		for n := 0; n < 105; n++ {
			g.Intents = append(g.Intents, board.Intent{ID: fmt.Sprintf("s%03d", n), From: []string{"origin"}, Description: "Observe fixture", Creator: "fixture", CreatedAt: tx.Now})
		}
		return tx.Save(g)
	}); err != nil {
		t.Fatal(err)
	}
	var metadata, first, next board.SchedulePage
	f.request("GET", f.base()+"/scheduling?protocol_version=1", nil, false, http.StatusOK, &metadata)
	if metadata.ExecutionChecks != nil {
		t.Fatal("metadata caller unexpectedly requested registry checks")
	}
	raw := f.request("GET", f.base()+"/scheduling?protocol_version=1&namespace=test", nil, false, http.StatusOK, &first)
	for _, omitted := range []string{`"intents":`, `"description":`, `"from":`, `"depends_on":`, `"support_valid":`} {
		if strings.Contains(raw, omitted) {
			t.Fatalf("scheduling leaked duplicate task or semantic content: %s", omitted)
		}
	}
	if len(first.Steps) != 100 || len(first.ExecutionChecks) != 101 || first.NextOffset != 100 {
		t.Fatalf("first page: steps=%d checks=%d next=%d", len(first.Steps), len(first.ExecutionChecks), first.NextOffset)
	}
	f.request("GET", f.base()+"/scheduling?protocol_version=1&namespace=test&offset=100&expected_version="+first.StateVersion, nil, false, http.StatusOK, &next)
	if len(next.Steps) != 5 || len(next.ExecutionChecks) != 5 || next.NextOffset != 0 {
		t.Fatalf("next page: steps=%d checks=%d next=%d", len(next.Steps), len(next.ExecutionChecks), next.NextOffset)
	}
	if check, ok := first.ExecutionChecks["reason:"]; !ok || check.Pending || check.Blocked {
		t.Fatalf("initial control admission missing or blocked: %+v", check)
	}
	if _, ok := first.ExecutionChecks["curate:"]; ok {
		t.Fatal("queried curation without pending observations")
	}
	for _, page := range []board.SchedulePage{first, next} {
		if page.ProtocolVersion != board.ScheduleProtocolVersion {
			t.Fatalf("page has incompatible scheduling protocol: %d", page.ProtocolVersion)
		}
		for _, step := range page.Steps {
			check, ok := page.ExecutionChecks["explore:"+step.ID]
			if !step.Ready || !ok || check.Blocked || check.Pending {
				t.Fatalf("missing or blocked new candidate %s: %+v", step.ID, check)
			}
		}
	}
	for _, namespace := range []string{"", strings.Repeat("n", 129)} {
		f.request("GET", f.base()+"/scheduling?protocol_version=1&namespace="+namespace, nil, false, http.StatusUnprocessableEntity, nil)
	}
	f.request("GET", f.base()+"/scheduling?protocol_version=1&namespace=test&expected_version=outdated", nil, false, http.StatusConflict, nil)
}

func TestSchedulingRejectsIncompatibleDispatchers(t *testing.T) {
	f, _ := newSnapshotHTTPFixture(t)
	before := f.state()
	for _, query := range []string{"", "protocol_version=", "protocol_version=0", "protocol_version=2", "protocol_version=01", "protocol_version=1&protocol_version=2"} {
		for _, namespace := range []string{"", "&namespace=test"} {
			raw := f.request("GET", f.base()+"/scheduling?"+query+namespace, nil, false, http.StatusUnprocessableEntity, nil)
			if !strings.Contains(raw, "scheduling protocol mismatch") || !strings.Contains(raw, "upgrade Server and Dispatcher together") {
				t.Fatalf("incompatible caller received no upgrade guidance: %s", raw)
			}
		}
	}
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("incompatible scheduling request changed persisted graph")
	}
	// Ordinary graph reads do not negotiate the scheduler's private projection.
	f.request("GET", f.base()+"/state", nil, false, http.StatusOK, nil)
}
