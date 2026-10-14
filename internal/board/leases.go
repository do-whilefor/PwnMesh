package board

import (
	"database/sql"
	"errors"
	"strings"
)

// LeaseOperation carries a decoded request. Authority and shared-state checks
// remain beside the lease update in the caller's Board transaction.
type LeaseOperation struct {
	Op, Worker, Trigger string
	Fence               ExecutionFence
	ExpectedVersion     *string
}

func (input LeaseOperation) validate() error {
	if strings.TrimSpace(input.Worker) == "" {
		return Err(422, "worker must be a non-empty string")
	}
	if input.Op == "claim" && strings.TrimSpace(input.Trigger) == "" {
		return Err(422, "trigger must be a non-empty string")
	}
	return nil
}

func (t *Tx) checkClaimIdentity(project, worker string) error {
	revoked, err := t.RunRevoked(project, worker)
	if err != nil {
		return err
	}
	if revoked {
		return Err(409, "Execution was revoked")
	}
	return nil
}

func (t *Tx) ReasonLease(project string, input LeaseOperation) (Project, error) {
	if input.Op != "claim" && input.Op != "heartbeat" && input.Op != "release" {
		return Project{}, Err(404, "Not Found")
	}
	if err := input.validate(); err != nil {
		return Project{}, err
	}
	fence := input.Fence
	if fence.Run != "" && (fence.Run != input.Worker || fence.Lease != "reason" || fence.Intent != "") {
		return Project{}, Err(403, "Planner lease identity mismatch")
	}
	g, err := t.Load(project)
	if err != nil {
		return Project{}, err
	}
	if err = g.RequireActive(); err != nil {
		return Project{}, err
	}
	if input.Op == "claim" || input.Op == "heartbeat" {
		if err = t.checkClaimIdentity(project, input.Worker); err != nil {
			return Project{}, err
		}
	}
	lease := g.Project.Reason
	if lease != nil && lease.Worker != input.Worker {
		return Project{}, Err(409, "Project reason is currently claimed by "+lease.Worker)
	}
	switch input.Op {
	case "claim":
		if lease == nil {
			g.Project.Reason = &Reason{Worker: input.Worker, Trigger: input.Trigger, StartedAt: t.Now, Heartbeat: t.Now}
		}
	case "heartbeat":
		if lease == nil {
			return Project{}, Err(409, "Project reason is not currently claimed")
		}
		if input.ExpectedVersion != nil {
			state, err := t.State(project)
			if err != nil {
				return Project{}, err
			}
			if *input.ExpectedVersion != DecisionStateVersion(state) {
				return Project{}, Err(409, "state_changed: decision input is no longer current; read the graph before deciding again")
			}
		}
		lease.Heartbeat = t.Now
	case "release":
		g.Project.Reason = nil
	}
	// A lease change must never re-save unrelated graph or state records.
	if reason := g.Project.Reason; reason != nil {
		_, err = t.Exec("UPDATE projects SET reason_worker=?,reason_trigger=?,reason_started_at=?,reason_last_heartbeat_at=? WHERE id=?", reason.Worker, reason.Trigger, reason.StartedAt, reason.Heartbeat, project)
	} else {
		_, err = t.Exec("UPDATE projects SET reason_worker=NULL,reason_trigger=NULL,reason_started_at=NULL,reason_last_heartbeat_at=NULL WHERE id=?", project)
	}
	return g.Project, err
}

func (t *Tx) IntentLease(project, intent string, input LeaseOperation) (Intent, error) {
	if input.Op != "heartbeat" && input.Op != "release" && input.Op != "conclude" {
		return Intent{}, Err(404, "Not Found")
	}
	if err := input.validate(); err != nil {
		return Intent{}, err
	}
	fence := input.Fence
	if fence.Run != "" && (fence.Lease != "explore" || fence.Run != input.Worker || fence.Intent != intent) {
		return Intent{}, Err(403, "Step operations require the matching Execute identity")
	}
	g, err := t.Load(project)
	if err != nil {
		return Intent{}, err
	}
	if err = g.RequireActive(); err != nil {
		return Intent{}, err
	}
	if input.Op == "conclude" {
		return Intent{}, Err(409, "Step completion requires the registered evidence result protocol")
	}
	if err = t.StepAvailable(project, intent); err != nil {
		return Intent{}, err
	}
	if input.Op == "heartbeat" {
		if err = t.checkClaimIdentity(project, input.Worker); err != nil {
			return Intent{}, err
		}
	}
	if err = t.CheckExecution(g, fence); err != nil {
		return Intent{}, err
	}
	for _, current := range g.Intents {
		if current.ID != intent {
			continue
		}
		if current.To != nil {
			return Intent{}, Err(409, "Intent already concluded")
		}
		if current.Worker != nil && *current.Worker != input.Worker {
			return Intent{}, Err(409, "Intent is currently claimed by "+*current.Worker)
		}
		if input.Op == "release" {
			current.Worker = nil
			_, err = t.Exec("UPDATE intents SET worker=NULL WHERE project_id=? AND id=?", project, intent)
			return current, err
		}
		if err = t.StepHeartbeatReady(project, intent, fence.Run); err != nil {
			if current.Worker != nil {
				return Intent{}, Err(409, "dependency_invalidated: "+err.Error())
			}
			return Intent{}, err
		}
		current.Worker, current.Heartbeat = Ptr(input.Worker), Ptr(t.Now)
		_, err = t.Exec("UPDATE intents SET worker=?,last_heartbeat_at=? WHERE project_id=? AND id=?", input.Worker, t.Now, project, intent)
		return current, err
	}
	return Intent{}, Err(404, "Intent not found")
}

func (t *Tx) CuratorLease(project string, input LeaseOperation) (Project, error) {
	if err := input.validate(); err != nil {
		return Project{}, err
	}
	fence := input.Fence
	if fence.Run != "" && (fence.Run != input.Worker || fence.Lease != "curate" || fence.Intent != "") {
		return Project{}, Err(403, "Curator lease identity mismatch")
	}
	if input.Op == "claim" || input.Op == "heartbeat" {
		if err := t.checkClaimIdentity(project, input.Worker); err != nil {
			return Project{}, err
		}
	}
	var err error
	switch input.Op {
	case "claim":
		_, err = t.ClaimCurator(project, input.Worker, input.Trigger)
	case "heartbeat":
		if input.ExpectedVersion != nil {
			state, stateErr := t.State(project)
			if stateErr != nil {
				return Project{}, stateErr
			}
			if err = state.Graph.RequireActive(); err != nil {
				return Project{}, err
			}
			fence := ExecutionFence{Run: input.Worker, Lease: "curate"}
			if err = t.CheckExecution(state.Graph, fence); err != nil {
				return Project{}, err
			}
			if err = t.CheckDecisionStateVersion(state, fence, *input.ExpectedVersion); err != nil {
				return Project{}, err
			}
		}
		err = t.HeartbeatCurator(project, input.Worker)
	case "release":
		err = t.ReleaseCurator(project, input.Worker)
	default:
		return Project{}, Err(404, "Not Found")
	}
	if err != nil {
		return Project{}, err
	}
	g, err := t.Load(project)
	return g.Project, err
}

func (t *Tx) CurationExecutionReceipt(project string, fence ExecutionFence) (StateActionResult, string, error) {
	if fence.Run == "" || fence.Lease != "curate" || fence.Intent != "" {
		return StateActionResult{}, "", Err(403, "Curation receipt requires its curator identity")
	}
	var status string
	err := t.QueryRow("SELECT status FROM pwnmesh_executions WHERE project_id=? AND lease=? AND kind='curate' AND intent=''", project, fence.Run).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return StateActionResult{}, "", Err(403, "Curation receipt requires a registered curator execution")
	}
	if err != nil {
		return StateActionResult{}, "", err
	}
	// A persisted receipt remains readable after lease release or revocation.
	receipt, err := t.CurationReceipt(project, fence.Run)
	return receipt, status, err
}
