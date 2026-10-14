//go:build linux

package worker

// The session owns phase policy; Loop holds the request-time mirror. These are
// the only writers of both phase representations. Prompts the Loop initializes
// itself are captured by saveAgentState at its settled persistence boundary.
func (r *sessionRuntime) applyPhase() {
	if r.loop == nil {
		return
	}
	r.loop.Concluding, r.loop.Repairing = r.state.Concluding, r.state.Repairing
	r.loop.TaskPrompt = r.state.TaskPrompt
	r.loop.ConclusionPrompt = r.state.ConclusionPrompt
	r.loop.RepairPrompt = r.state.RepairPrompt
}

func (r *sessionRuntime) setConcluding() {
	if r.loop != nil {
		r.state.TaskPrompt = r.loop.TaskPrompt
	}
	r.state.Concluding = true
	r.applyPhase()
}

func (r *sessionRuntime) setRepair(repairing bool, prompt string, pending bool) {
	r.state.Repairing, r.state.RepairPrompt, r.state.RepairPending = repairing, prompt, pending
	r.applyPhase()
}
