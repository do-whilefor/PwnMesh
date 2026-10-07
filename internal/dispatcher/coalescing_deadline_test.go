package dispatcher

import (
	"context"
	"testing"
	"time"

	"pwnmesh/internal/board"
)

func TestQuietWaitSchedulesEarliestBoundaryWithoutBusyRetries(t *testing.T) {
	now := time.Unix(1000, 0)
	s := &Scheduler{}
	if !s.waitForQuiet(reasonWait{First: now, Changed: now}, now) || !s.nextWake.Equal(now.Add(reasonQuietPeriod)) {
		t.Fatalf("quiet boundary = %s", s.nextWake)
	}
	// A different project approaching its maximum wait must wake first,
	// even though fresh observations keep extending its quiet window.
	wait := reasonWait{First: now.Add(-reasonMaxWait + time.Second), Changed: now}
	if !s.waitForQuiet(wait, now) || !s.nextWake.Equal(now.Add(time.Second)) {
		t.Fatalf("maximum wait did not bound the next wake: %s", s.nextWake)
	}
	if !s.waitForQuiet(reasonWait{First: now, Changed: now}, now) || !s.nextWake.Equal(now.Add(time.Second)) {
		t.Fatal("a later project postponed the earliest pending boundary")
	}
	for _, wait := range []reasonWait{
		{First: now.Add(-reasonMaxWait), Changed: now},
		{First: now.Add(-reasonQuietPeriod), Changed: now.Add(-reasonQuietPeriod)},
		{First: now, Changed: now, Urgent: true},
	} {
		s.nextWake = time.Time{}
		if s.waitForQuiet(wait, now) || !s.nextWake.IsZero() {
			t.Fatalf("ready work scheduled a repeated immediate wake: %+v", wait)
		}
	}
}

func TestRunWakesAtCurationQuietDeadline(t *testing.T) {
	s, runner, _, graph, ctx := pipelineFixture(t, 3, false)
	runner.reconcile = true
	// Configure before starting producers: heartbeat goroutines read Interval.
	s.Config.Runtime.Interval = 60
	if err := s.Client.Do(context.Background(), "PUT", "/settings", board.Settings{IntentTimeout: 180, ReasonTimeout: 180}, nil, nil); err != nil {
		t.Fatal(err)
	}
	s.leaseTimeout = 180 * time.Second
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	// Both observations are durable, while their producers stay in flight.
	for range 2 {
		pipelineStarted(t, ctx, runner)
	}
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	wait, ok := s.curationWaits[graph.Project.ID]
	if !ok {
		t.Fatal("pending producer observations were not coalesced")
	}
	// Advance only this already-established quiet window. The long heartbeat
	// makes a missed deadline fail within the existing bounded assertion.
	wait.Changed = time.Now().Add(-reasonQuietPeriod + 150*time.Millisecond)
	wait.First = wait.Changed
	s.curationWaits[graph.Project.ID] = wait
	stop := runWakeupScheduler(t, s)
	if job := nextWakeupJob(t, runner.curateStarted); job.Kind != "curate" {
		t.Fatalf("quiet deadline launched %s", job.Kind)
	}
	stop()
}
