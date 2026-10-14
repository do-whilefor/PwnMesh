package board

import (
	"database/sql"
	"encoding/json"
	"errors"

	"pwnmesh/internal/board/internal/sqlstore"
	"pwnmesh/internal/board/model"
)

// DecisionStateVersion binds a plan to shared content rather than lease
// heartbeats or event counters. Copy slices before removing runtime metadata.
func DecisionStateVersion(state State) string { return model.DecisionStateVersion(state) }

// DecisionJobVersion returns the initial version for the new decision protocol.
// A malformed non-null marker must never silently opt a job into legacy writes.
func DecisionJobVersion(raw json.RawMessage) (string, error) {
	var job struct {
		Kind          string         `json:"kind"`
		Graph         Graph          `json:"graph"`
		State         *State         `json:"state"`
		InputSnapshot *InputSnapshot `json:"input_snapshot"`
		Decision      *struct {
			Version         int    `json:"version"`
			StateVersion    string `json:"state_version"`
			ClosureProtocol int    `json:"closure_protocol,omitempty"`
		} `json:"decision"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return "", Err(422, "invalid decision job")
	}
	if job.Decision == nil {
		return "", nil
	}
	if job.Decision.ClosureProtocol != 0 && (job.Decision.ClosureProtocol != 1 || job.Decision.Version != 2) {
		return "", Err(422, "unsupported closure protocol")
	}
	if ref := job.InputSnapshot; ref != nil {
		if job.State != nil || job.Kind != "reason" || (job.Decision.Version != 1 && job.Decision.Version != 2) || len(ref.ID) != 64 || len(ref.StateVersion) != 64 || ref.ProjectID != job.Graph.Project.ID || ref.Generation != job.Graph.Project.Generation || job.Decision.StateVersion != ref.StateVersion {
			return "", Err(422, "invalid decision snapshot binding")
		}
		return ref.StateVersion, nil
	}
	if job.Kind != "reason" || (job.Decision.Version != 1 && job.Decision.Version != 2) || job.Decision.StateVersion == "" || job.State == nil {
		return "", Err(422, "decision requires version 1 or 2 and a bound state snapshot")
	}
	version := DecisionStateVersion(*job.State)
	if job.Decision.StateVersion != version {
		return "", Err(422, "decision state_version does not match its saved state")
	}
	state := *job.State
	state.Graph = job.Graph
	if DecisionStateVersion(state) != version {
		return "", Err(422, "decision state and job graph do not match")
	}
	return version, nil
}

// CheckDecisionStateVersion runs inside the same transaction as the write.
// Old clients may supply a version voluntarily; new registered Decide jobs
// must supply one. Existing lease and source checks remain independent.
func (t *Tx) CheckDecisionStateVersion(state State, fence ExecutionFence, expected string) error {
	if fence.Lease == "curate" {
		_, raw, err := sqlstore.ExecutionInputByLease(t, state.Graph.Project.ID, fence.Run, "curate")
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Err(403, "curation requires a registered input snapshot")
			}
			return err
		}
		version, _, err := CurationJobBoundary(raw)
		if err != nil {
			return err
		}
		if expected != version {
			return Err(409, "curation input version does not match its registered snapshot")
		}
	}
	if fence.Lease == "reason" {
		_, raw, err := sqlstore.ExecutionInputByLease(t, state.Graph.Project.ID, fence.Run, "reason")
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			version, err := DecisionJobVersion(raw)
			if err != nil {
				return err
			}
			if version != "" && expected == "" {
				return Err(422, "decision write requires expected_version")
			}
		}
	}
	if expected != "" && expected != DecisionStateVersion(state) {
		return Err(409, "state_changed: decision input is no longer current; read the graph before deciding again")
	}
	return nil
}
