package phase

import "testing"

func TestTurnDecisionsPreserveControlAndConclusionRestrictions(t *testing.T) {
	for _, test := range []struct {
		name  string
		state State
		turn  Turn
		want  Action
	}{
		{"execution tools", State{}, Turn{HasCalls: true, InvalidResult: true}, KeepRunning},
		{"completed", State{}, Turn{}, KeepRunning},
		{"continue", State{}, Turn{Continue: true}, Continue},
		{"bad output", State{}, Turn{InvalidResult: true}, Repair},
		{"truncated tools", State{}, Turn{HasCalls: true, Truncated: true, InvalidResult: true}, Repair},
		{"deadline after tools", State{}, Turn{HasCalls: true, InvalidResult: true, ConcludeDue: true}, Conclude},
		{"deadline after invalid result", State{}, Turn{InvalidResult: true, ConcludeDue: true}, ConcludeAndRepair},
		{"deadline beats continue", State{}, Turn{Continue: true, ConcludeDue: true}, Conclude},
		{"conclusion forbids tools", State{Concluding: true}, Turn{HasCalls: true, InvalidResult: true, ConcludeDue: true}, Repair},
		{"repair forbids tools", State{Repairing: true}, Turn{HasCalls: true, InvalidResult: true}, Repair},
		{"control never concludes", State{}, Turn{Control: true, ConcludeDue: true}, KeepRunning},
		{"native tools", State{}, Turn{NativeControl: true, HasCalls: true}, KeepRunning},
		{"native rejection", State{}, Turn{NativeControl: true, Rejected: true}, KeepRunning},
		{"native text is not a receipt", State{}, Turn{NativeControl: true}, Continue},
		{"native truncated rejection", State{}, Turn{NativeControl: true, Rejected: true, Truncated: true}, Continue},
		{"native truncated tools", State{}, Turn{NativeControl: true, HasCalls: true, Truncated: true}, Continue},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.state.AfterTurn(test.turn); got != test.want {
				t.Fatalf("next action = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRepairRecoveryDistinguishesTransportFromUnansweredCrash(t *testing.T) {
	pending := State{Repairing: true, RepairPending: true, RepairCount: 1}
	for _, test := range []struct {
		state          State
		prompt         bool
		infrastructure bool
		want           RepairRecovery
	}{
		{State{}, false, false, NoRepairRecovery},
		{State{Repairing: true}, true, true, NoRepairRecovery},
		{pending, false, false, PrepareRepairRequest},
		{pending, false, true, PrepareRepairRequest},
		{pending, true, true, ReuseRepairRequest},
		{pending, true, false, RetryRepairRequest},
	} {
		if got := test.state.ResumeRepair(test.prompt, test.infrastructure); got != test.want {
			t.Fatalf("resume action = %v, want %v for %+v", got, test.want, test)
		}
	}
}
