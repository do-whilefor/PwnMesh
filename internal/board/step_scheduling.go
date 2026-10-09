package board

// ScheduleStep is the current scheduling projection. Historical Intent fences
// are resolved server-side; dispatchers need neither a second task list nor
// descriptions and evidence. Ready still requires an execution-registry grant.
type ScheduleStep struct {
	ID             string   `json:"id"`
	Status         string   `json:"status"`
	Priority       int      `json:"priority"`
	CreatedAt      string   `json:"created_at"`
	Ready          bool     `json:"ready"`
	Running        bool     `json:"running,omitempty"`
	InvalidSources []string `json:"invalid_sources,omitempty"`
	BlockedBy      []string `json:"blocked_by,omitempty"`
	WritePaths     []string `json:"-"` // Server-side admission only; checks carry the result.
}

// Preserve persisted task order and legacy fences at this one boundary. Failed
// Steps remain candidates because only the execution registry authorizes retry.
// Missing projected Steps never authorize new execution.
func scheduleSteps(intents []Intent, steps []Step) []ScheduleStep {
	byID := make(map[string]Step, len(steps))
	for _, step := range steps {
		byID[step.ID] = step
	}
	projected := make([]ScheduleStep, 0, len(intents))
	for _, intent := range intents {
		step, exists := byID[intent.ID]
		open := intent.To == nil && intent.ConcludedAt == nil
		projected = append(projected, ScheduleStep{
			ID: intent.ID, Status: step.Status, Priority: step.Priority, CreatedAt: intent.CreatedAt,
			Ready: exists && open && intent.Worker == nil && !pendingBootstrap(intent) &&
				(step.Status == "open" || step.Status == "failed") && len(step.InvalidSources) == 0 && len(step.BlockedBy) == 0,
			Running:        open && intent.Worker != nil,
			InvalidSources: step.InvalidSources[:min(1, len(step.InvalidSources))], BlockedBy: step.BlockedBy,
			WritePaths: stepWritePaths(step),
		})
	}
	return projected
}

func pendingBootstrap(i Intent) bool {
	return i.To == nil && i.ConcludedAt == nil && i.Description == "bootstrap" && i.Creator == "dispatcher.bootstrap" && len(i.From) == 1 && i.From[0] == "origin"
}
