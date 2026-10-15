package board

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"pwnmesh/internal/board/internal/sqlstore"
	"pwnmesh/internal/board/model"
)

func (t *Tx) FreezeInput(state State) (out *InputSnapshot, err error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	ref := &InputSnapshot{Version: 1, ID: hex.EncodeToString(sum[:]), ProjectID: state.Graph.Project.ID,
		Generation: state.Graph.Project.Generation, Revision: state.Revision, DecisionRevision: state.DecisionRevision,
		StateVersion: DecisionStateVersion(state), FactCount: len(state.Graph.Facts), HintCount: len(state.Graph.Hints), OpenCount: state.Graph.OpenCount(), StepCount: len(state.Steps)}
	metadata, err := json.Marshal(ref)
	if err != nil {
		return nil, err
	}
	err = sqlstore.SaveInputSnapshot(t, ref.ProjectID, ref.ID, metadata, raw)
	return ref, err
}

func (t *Tx) InputSnapshotMetadata(project, id string) (*InputSnapshot, error) {
	raw, err := sqlstore.ReadInputSnapshotMetadata(t, project, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Err(404, "Input snapshot not found")
	}
	if err != nil {
		return nil, err
	}
	var ref InputSnapshot
	err = json.Unmarshal(raw, &ref)
	return &ref, err
}

func (t *Tx) ReadInputSnapshot(project, id string) (State, error) {
	raw, err := t.readInputSnapshotBytes(project, id)
	if err != nil {
		return State{}, err
	}
	var state State
	err = json.Unmarshal(raw, &state)
	return state, err
}

func (t *Tx) readInputSnapshotBytes(project, id string) ([]byte, error) {
	raw, err := sqlstore.ReadInputSnapshot(t, project, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Err(404, "Input snapshot not found")
	}
	return raw, err
}

// DecisionRetryKey preserves existing retry identity while its computation
// moves next to the authoritative graph, away from the dispatcher.
func DecisionRetryKey(g Graph, revision int64) string {
	ended := []string{}
	for _, i := range g.Intents {
		if i.To != nil || i.ConcludedAt != nil {
			ended = append(ended, i.ID)
		}
	}
	raw, _ := json.Marshal(struct {
		Facts    []Fact
		Hints    []Hint
		Ended    []string
		Revision int64
	}{g.Facts, g.Hints, ended, revision})
	sum := sha256.Sum256(raw)
	return "reason:" + hex.EncodeToString(sum[:])
}

// ScheduleProtocolVersion identifies the normalized Step scheduling projection.
// Server and Dispatcher must agree before treating absent fields as idle work.
const ScheduleProtocolVersion = model.ScheduleProtocolVersion

// SchedulePage contains runtime metadata only. Node descriptions, evidence,
// hints and historical Jobs never cross the daily scheduling boundary.
type SchedulePage = model.SchedulePage

func (t *Tx) ScheduleInput(project string, offset int, expected string) (SchedulePage, error) {
	if offset < 0 {
		return SchedulePage{}, Err(422, "invalid scheduling offset")
	}
	header, count, err := sqlstore.ScheduleHeader(t, project)
	if err == nil {
		var page SchedulePage
		if err := json.Unmarshal(header, &page); err != nil {
			return SchedulePage{}, err
		}
		if expected != "" && expected != page.StateVersion {
			return SchedulePage{}, Err(409, "state_changed: scheduling input changed")
		}
		if offset > count {
			return SchedulePage{}, Err(422, "invalid scheduling offset")
		}
		page.Steps, err = sqlstore.ScheduleSteps(t, project, offset, 100)
		if err == nil && len(page.Steps) != min(100, count-offset) {
			return SchedulePage{}, errors.New("incomplete scheduling projection")
		}
		if offset+len(page.Steps) < count {
			page.NextOffset = offset + len(page.Steps)
		}
		return page, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SchedulePage{}, err
	}
	s, err := t.State(project)
	if err != nil {
		return SchedulePage{}, err
	}
	version := DecisionStateVersion(s)
	if expected != "" && expected != version {
		return SchedulePage{}, Err(409, "state_changed: scheduling input changed")
	}
	if offset > len(s.Graph.Intents) {
		return SchedulePage{}, Err(422, "invalid scheduling offset")
	}
	p := SchedulePage{ProtocolVersion: ScheduleProtocolVersion, Project: s.Graph.Project, FactCount: len(s.Graph.Facts), HintCount: len(s.Graph.Hints), OpenCount: s.Graph.OpenCount(), Revision: s.Revision, DecisionRevision: s.DecisionRevision, StateVersion: version, RetryKey: DecisionRetryKey(s.Graph, s.DecisionRevision)}
	p.CurationRequested = s.PendingCurationRequest() != nil
	if p.CurationNeeded, err = t.curationNeeded(s); err != nil {
		return SchedulePage{}, err
	}
	if s.Graph.Project.OrchestrationVersion == 1 {
		p.CurationRetryKey = CurationRetryKey(s)
	}
	p.Initial = len(s.Graph.Facts) == 2
	origin, goal := false, false
	for _, f := range s.Graph.Facts {
		origin = origin || f.ID == "origin"
		goal = goal || f.ID == "goal"
	}
	p.Initial = p.Initial && origin && goal
	for _, i := range s.Graph.Intents {
		p.Initial = p.Initial && pendingBootstrap(i)
	}
	steps := scheduleSteps(s.Graph.Intents, s.Steps)
	// Cold views remain observational. The scheduling endpoint uses Do to
	// expire leases and atomically publish a reusable projection.
	if !t.readOnly {
		header, err := json.Marshal(p)
		if err != nil {
			return SchedulePage{}, err
		}
		if err := sqlstore.SaveSchedule(t, project, header, steps); err != nil {
			return SchedulePage{}, err
		}
	}
	end := offset + min(100, len(steps)-offset)
	p.Steps = steps[offset:end]
	if end < len(s.Graph.Intents) {
		p.NextOffset = end
	}
	return p, nil
}
