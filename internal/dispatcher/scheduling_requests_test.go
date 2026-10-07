package dispatcher

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"pwnmesh/internal/board"
)

func TestDispatchCandidateRequestCounts(t *testing.T) {
	s, _, _, _ := automaticRetryFixture(t, 0, "")
	var reason, explore atomic.Int32
	s.Client.HTTP = &http.Client{Transport: executionQueryTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/executions/check") {
			switch r.URL.Query().Get("kind") {
			case "reason":
				reason.Add(1)
			case "explore":
				explore.Add(1)
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	retryTicks(t, s, 1)
	if got := reason.Load(); got != 0 {
		t.Fatalf("initial Decide checks = %d", got)
	}
	reason.Store(0)
	retryTicks(t, s, 1)
	if got := explore.Load(); got != 0 {
		t.Fatalf("chosen Execute checks = %d", got)
	}
	t.Logf("standalone candidate checks: Decide=%d, Execute=%d", reason.Load(), explore.Load())
}

func TestSchedulingControlChecksMatchFallbackWithoutExtraRequests(t *testing.T) {
	s, _, _, graph := pendingCurationFixture(t)
	ctx := context.Background()
	var requests atomic.Int32
	s.Client.HTTP = &http.Client{Transport: executionQueryTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/executions/check") {
			requests.Add(1)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	input, err := s.scheduleInput(ctx, graph.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.schedules[graph.Project.ID] = input
	if !input.CurationNeeded {
		t.Fatal("fixture requires pending curation")
	}
	for _, kind := range []string{"reason", "curate"} {
		before := requests.Load()
		check, err := s.candidateCheck(ctx, input, graph, kind, nil)
		if err != nil || requests.Load() != before {
			t.Fatalf("%s scheduling repeated a registry request: %v", kind, err)
		}
		// Callers without a scheduling snapshot retain the direct query.
		fallback, err := s.candidateCheck(ctx, board.SchedulePage{}, graph, kind, nil)
		if err != nil || requests.Load() != before+1 || !reflect.DeepEqual(check, fallback) {
			t.Fatalf("%s snapshot differs from direct admission: snapshot=%+v fallback=%+v err=%v", kind, check, fallback, err)
		}
	}
	requests.Store(0)
	retryTicks(t, s, 4)
	if requests.Load() != 0 {
		t.Fatalf("control dispatch made %d redundant registry requests", requests.Load())
	}
}

func TestRetryKeyPreservesLegacyBytes(t *testing.T) {
	s, _, _, _ := automaticRetryFixture(t, 0, "")
	g := board.Graph{Project: board.Project{ID: "fixture"}}
	if got := s.retryKey(g, "reason", nil); got != "reason:6131a7e2e21f30d058650b89faec6a12380760e4c8ef0d39b481400bb08c2ac0" {
		t.Fatalf("nil collection identity changed: %s", got)
	}
	g.Facts, g.Hints = []board.Fact{}, []board.Hint{}
	g.Intents = []board.Intent{{ID: "open"}, {ID: "done", To: board.Ptr("fact")}, {ID: "abandoned", ConcludedAt: board.Ptr("now")}}
	s.stateRevisions[g.Project.ID] = 7
	if got := s.retryKey(g, "reason", nil); got != "reason:a86d0bc22e11e5da882fdc78a134a95cafd08467213a711605afcc547492a35d" {
		t.Fatalf("ordered completion identity changed: %s", got)
	}
}
