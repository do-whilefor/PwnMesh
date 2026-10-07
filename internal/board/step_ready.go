package board

// StepReady is checked at claim, registration and process start, each inside
// the write transaction. Raw observations remain available after invalidation,
// while new execution and success require valid registered prerequisites.
func (t *Tx) StepReady(project, id string) error {
	return t.stepReady(project, id, "")
}

// StepHeartbeatReady lets an already running, fenced Execute retain independent
// observations after a From premise is corrected. Accepted depends_on results
// remain mandatory; a claim or restart still needs every premise to be valid.
func (t *Tx) StepHeartbeatReady(project, id, lease string) error {
	return t.stepReady(project, id, lease)
}

func (t *Tx) stepReady(project, id, lease string) error {
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
			if len(stepWritePaths(step)) != 0 {
				blocked, err := t.stepWriteBlocked(project, id)
				if err != nil {
					return err
				}
				if blocked {
					return Err(409, "write_conflict: another Step reserves an overlapping write path")
				}
			}
			premiseErr := s.ValidateFactSources(step.From, false)
			if premiseErr == nil || lease == "" {
				return premiseErr
			}
			var running bool
			if err := t.QueryRow(`SELECT EXISTS(SELECT 1 FROM xloom_executions WHERE project_id=? AND intent=? AND lease=? AND kind='explore' AND status IN ('running','result_pending'))`, project, id, lease).Scan(&running); err != nil {
				return err
			}
			if !running {
				return premiseErr
			}
			return nil
		}
	}
	return Err(404, "Step not found")
}
