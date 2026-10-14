package phase

// Turn describes a settled model response without importing model protocols or
// result contracts. The adapter owns parsing; this package owns what follows.
type Turn struct {
	Control       bool
	NativeControl bool
	HasCalls      bool
	Truncated     bool
	InvalidResult bool
	Rejected      bool
	Continue      bool
	ConcludeDue   bool
}

type Action uint8

const (
	KeepRunning Action = iota
	Conclude
	ConcludeAndRepair
	Repair
	Continue
)

func (s State) AfterTurn(turn Turn) Action {
	if turn.NativeControl {
		if !turn.Truncated && (turn.HasCalls || turn.Rejected) {
			return KeepRunning
		}
		return Continue
	}
	needsResult := !turn.HasCalls || turn.Truncated || s.Concluding || s.Repairing
	if !s.Concluding && !turn.Control && turn.ConcludeDue {
		if needsResult && turn.InvalidResult {
			return ConcludeAndRepair
		}
		return Conclude
	}
	if needsResult && turn.InvalidResult {
		return Repair
	}
	if needsResult && turn.Continue {
		return Continue
	}
	return KeepRunning
}

type RepairRecovery uint8

const (
	NoRepairRecovery RepairRecovery = iota
	ReuseRepairRequest
	PrepareRepairRequest
	RetryRepairRequest
)

func (s State) ResumeRepair(promptInHistory, infrastructureResume bool) RepairRecovery {
	if !s.Repairing || !s.RepairPending {
		return NoRepairRecovery
	}
	if !promptInHistory {
		return PrepareRepairRequest
	}
	if infrastructureResume {
		return ReuseRepairRequest
	}
	return RetryRepairRequest
}
