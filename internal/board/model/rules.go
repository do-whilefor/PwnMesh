package model

// RequireActive gates exploration writes; project management and human Hints
// have their own state rules and deliberately do not call it.
func (g Graph) RequireActive() error {
	if g.Project.Status != "active" {
		return Err(403, "Project is "+g.Project.Status)
	}
	return nil
}

func (g Graph) ValidateSources(from []string) error {
	for _, id := range from {
		found := false
		for _, f := range g.Facts {
			if f.ID == id {
				found = true
				break
			}
		}
		if !found {
			return Err(404, "Fact "+id+" not found")
		}
	}
	for _, id := range from {
		if id == "goal" {
			return Err(400, "goal cannot be used in from")
		}
	}
	return nil
}

// ExecutionFence adds conditional writes for the dispatcher. An empty Run
// retains the public Cairn semantics for existing clients.
type ExecutionFence struct {
	Run, Lease, Intent string
}
