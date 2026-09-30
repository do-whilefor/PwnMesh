package board

// StepReady is checked at claim, registration and process start, each inside
// the write transaction. Raw observations remain available after invalidation,
// while success and continued execution require valid registered prerequisites.
func (t *Tx) StepReady(project, id string) error {
	s, err := t.State(project)
	if err != nil {
		return err
	}
	for _, step := range s.Steps {
		if step.ID == id {
			if step.Status == "abandoned" {
				return Err(409, "Step was abandoned")
			}
			if len(step.BlockedBy) != 0 {
				return Err(409, "Step dependency "+step.BlockedBy[0]+" is not ready")
			}
			return s.ValidateFactSources(step.From, false)
		}
	}
	return Err(404, "Step not found")
}
