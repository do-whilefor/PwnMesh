package board

// ExecutionSteps is the scheduling adapter for retained Intent records. The
// dispatcher orders Steps; legacy conclusion and lease fields stay at this
// boundary. Failed Steps remain candidates because the execution registry owns
// retry authorization. A missing projected Step never authorizes new execution.
func ExecutionSteps(intents []Intent, steps []Step) []Step {
	byID := make(map[string]Step, len(steps))
	for _, step := range steps {
		byID[step.ID] = step
	}
	var candidates []Step
	for _, intent := range intents {
		step, exists := byID[intent.ID]
		if !exists || pendingBootstrap(intent) || intent.To != nil || intent.ConcludedAt != nil || intent.Worker != nil ||
			(step.Status != "open" && step.Status != "failed") || len(step.InvalidSources) != 0 || len(step.BlockedBy) != 0 {
			continue
		}
		step.CreatedAt = intent.CreatedAt
		candidates = append(candidates, step)
	}
	return candidates
}

func pendingBootstrap(i Intent) bool {
	return i.To == nil && i.ConcludedAt == nil && i.Description == "bootstrap" && i.Creator == "dispatcher.bootstrap" && len(i.From) == 1 && i.From[0] == "origin"
}
