package dispatcher

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
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
	var input board.SchedulePage
	var requests atomic.Int32
	s.Client.HTTP = &http.Client{Transport: executionQueryTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/executions/check") {
			requests.Add(1)
			key := input.RetryKey
			if r.URL.Query().Get("kind") == "curate" {
				key = input.CurationRetryKey
			}
			if r.URL.Query().Get("retry_key") != key || r.URL.Query().Get("generation") != strconv.FormatInt(input.Project.Generation, 10) {
				t.Errorf("direct admission lost its current scheduling identity: %s", r.URL)
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	input, err := s.scheduleInput(ctx, graph.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.schedules[graph.Project.ID] = board.SchedulePage{RetryKey: "stale-reason", CurationRetryKey: "stale-curation"}
	if !input.CurationNeeded {
		t.Fatal("fixture requires pending curation")
	}
	for _, kind := range []string{"reason", "curate"} {
		before := requests.Load()
		check, err := s.candidateCheck(ctx, input, kind, "")
		if err != nil || requests.Load() != before {
			t.Fatalf("%s scheduling repeated a registry request: %v", kind, err)
		}
		// A page without precomputed checks retains direct admission using its
		// own identity, even if the previous cached page is already stale.
		withoutChecks := input
		withoutChecks.ExecutionChecks = nil
		fallback, err := s.candidateCheck(ctx, withoutChecks, kind, "")
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

func TestSchedulingPagesKeepRemoteLeaseAndCreationOrder(t *testing.T) {
	s, _, store, graph := automaticRetryFixture(t, 0, "")
	ctx := context.Background()
	if err := store.Do(ctx, func(tx *board.Tx) error {
		g, err := tx.Load(graph.Project.ID)
		if err != nil {
			return err
		}
		for n := 0; n < 105; n++ {
			intent := board.Intent{ID: fmt.Sprintf("s%03d", n), From: []string{"origin"}, Description: "Observe fixture", Creator: "fixture", CreatedAt: tx.Now}
			if n == 100 {
				intent.Worker, intent.Heartbeat = board.Ptr("other-dispatcher@run"), board.Ptr(tx.Now)
			}
			g.Intents = append(g.Intents, intent)
		}
		return tx.Save(g)
	}); err != nil {
		t.Fatal(err)
	}
	input, err := s.scheduleInput(ctx, graph.Project.ID)
	if err != nil || len(input.Steps) != 105 {
		t.Fatalf("lost scheduling page: steps=%d err=%v", len(input.Steps), err)
	}
	for n, step := range input.Steps {
		if step.ID != fmt.Sprintf("s%03d", n) || step.CreatedAt == "" || step.Ready != (n != 100) || step.Running != (n == 100) {
			t.Fatalf("page changed lease/readiness/ordering at %d: %+v", n, step)
		}
		_, checked := input.ExecutionChecks["explore:"+step.ID]
		if checked != step.Ready {
			t.Fatalf("registry admission disagrees with page candidate %s", step.ID)
		}
	}
	s.schedules[graph.Project.ID] = input
	if !s.producersRunning(board.Graph{Project: input.Project}) {
		t.Fatal("planner ignored another dispatcher's lease on a later page")
	}
}
