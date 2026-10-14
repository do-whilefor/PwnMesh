package phase

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDeadlinesRemainBoundAcrossPhaseAndRecovery(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	budget := Budget{Execution: time.Minute, Conclusion: 20 * time.Second}
	execute := New(start, budget)
	if err := execute.ValidateDeadlines(budget); err != nil {
		t.Fatal(err)
	}
	if execute.ShouldConclude(start.Add(59*time.Second), false, false) || !execute.ShouldConclude(start.Add(time.Minute), false, false) || !execute.ShouldConclude(start, false, true) || execute.ShouldConclude(start.Add(time.Hour), true, true) {
		t.Fatal("execution deadline, soft stop or control-job admission changed")
	}
	conclude, err := execute.BeginConclusion(start.Add(time.Minute), budget)
	if err != nil {
		t.Fatal(err)
	}
	if conclude.ExecutionDeadline != execute.ExecutionDeadline || conclude.ConcludeDeadline != start.Add(80*time.Second) || !conclude.Concluding || execute.Concluding {
		t.Fatal("conclusion changed its input or original execution budget")
	}
	resumed, err := conclude.BeginConclusion(start.Add(70*time.Second), budget)
	if err != nil || resumed != conclude {
		t.Fatalf("recovery refreshed conclusion state: %+v %v", resumed, err)
	}
	if expired, err := conclude.BeginConclusion(start.Add(80*time.Second), budget); !errors.Is(err, context.DeadlineExceeded) || expired != conclude {
		t.Fatalf("expired conclusion received another budget: %+v %v", expired, err)
	}
	if conclude.ShouldConclude(start.Add(time.Hour), false, true) {
		t.Fatal("conclusion reentered after soft stop")
	}
	control := execute.BoundControl()
	if control.ReasonDeadline != execute.ExecutionDeadline {
		t.Fatal("control phase did not inherit its original execution budget")
	}
	control.ReasonDeadline = start.Add(30 * time.Second)
	if control.BoundControl() != control {
		t.Fatal("control recovery lengthened a saved shorter deadline")
	}
	if unlimited := New(start, Budget{}); !unlimited.ExecutionDeadline.IsZero() || unlimited.ShouldConclude(start.Add(time.Hour), false, false) {
		t.Fatal("unlimited execution acquired a deadline")
	}
}

func TestSavedPhaseValidationRejectsInventedAllowancesAndDeadlines(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	budget := Budget{Execution: time.Minute, Conclusion: time.Second}
	valid := New(start, budget)
	for name, mutate := range map[string]func(*State){
		"missing execution":  func(s *State) { s.ExecutionDeadline = time.Time{} },
		"extended execution": func(s *State) { s.ExecutionDeadline = s.ExecutionDeadline.Add(time.Second) },
		"missing conclusion": func(s *State) { s.Concluding = true },
		"extended control":   func(s *State) { s.ReasonDeadline = s.ExecutionDeadline.Add(time.Second) },
		"extended conclusion": func(s *State) {
			s.Concluding = true
			s.ConcludeStartedAt = start
			s.ConcludeDeadline = start.Add(time.Minute)
		},
	} {
		t.Run(name, func(t *testing.T) {
			saved := valid
			mutate(&saved)
			if err := saved.ValidateDeadlines(budget); err == nil {
				t.Fatal("invalid saved deadline was accepted")
			}
		})
	}
	for _, saved := range []State{{RepairCount: -1}, {RepairCount: MaxOutputRepairs + 1}, {Repairing: true}, {RepairPending: true}, {ContinuationCount: -1}, {ContinuationCount: MaxContinuations + 1}} {
		if err := saved.ValidateAllowances(false); err == nil {
			t.Fatalf("invalid saved allowance was accepted: %+v", saved)
		}
	}
	if err := valid.ValidateAllowances(true); err == nil {
		t.Fatal("repair prompt without repair phase was accepted")
	}
	if _, err := (State{Concluding: true}).BeginConclusion(start, budget); err == nil {
		t.Fatal("recovery manufactured a missing conclusion start time")
	}
}

func TestRepairContinuationAndProgressPreserveIndependentBudgets(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	budget := Budget{Execution: time.Minute, Conclusion: time.Minute}
	initial, err := New(start, budget).BeginConclusion(start.Add(time.Minute), budget)
	if err != nil {
		t.Fatal(err)
	}
	repair, err := initial.BeginRepair()
	if err != nil || !repair.Repairing || !repair.RepairPending || repair.RepairCount != 1 || !repair.Concluding || initial.RepairCount != 0 {
		t.Fatalf("repair lost conclusion or mutated its prior value: %+v %v", repair, err)
	}
	if err := repair.ValidateAllowances(true); err != nil {
		t.Fatal(err)
	}
	responded := repair.RepairResponded()
	if responded.RepairPending || !responded.Repairing || responded.RepairCount != repair.RepairCount {
		t.Fatal("durable response replenished the repair budget")
	}
	continued, err := responded.Continue(11)
	if err != nil || continued.Repairing || continued.RepairPending || continued.RepairCount != 1 || continued.ContinuationCount != 1 || !continued.Concluding || continued.ConcludeDeadline != initial.ConcludeDeadline {
		t.Fatalf("continuation refreshed a deadline or repair budget: %+v %v", continued, err)
	}
	for sequence := uint64(12); sequence <= 13; sequence++ {
		continued, err = continued.Continue(sequence)
		if err != nil {
			t.Fatal(err)
		}
	}
	if replay, err := continued.Continue(13); err != nil || replay != continued {
		t.Fatalf("recovery charged the same assistant sequence again: %+v %v", replay, err)
	}
	if rejected, err := continued.Continue(14); !errors.Is(err, ErrContinuationExhausted) || rejected != continued {
		t.Fatalf("continuation exhaustion mutated state: %+v %v", rejected, err)
	}
	if _, err := continued.Continue(0); err == nil {
		t.Fatal("unsequenced assistant response received continuation")
	}
	progress := continued.SuccessfulProgress()
	want := continued
	want.ContinuationCount = 0
	if progress != want {
		t.Fatal("tool progress changed more than the continuation allowance")
	}
	second, err := progress.BeginRepair()
	if err != nil || second.RepairCount != 2 {
		t.Fatalf("remaining repair slot was lost: %+v %v", second, err)
	}
	if rejected, err := second.BeginRepair(); !errors.Is(err, ErrRepairExhausted) || rejected != second {
		t.Fatalf("repair exhaustion mutated state: %+v %v", rejected, err)
	}
}
