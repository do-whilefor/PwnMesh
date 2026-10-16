package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	b "pwnmesh/internal/board"
	"pwnmesh/internal/execution"
	"pwnmesh/internal/protocol"
)

func (s *Server) schedulingInput(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	versions := r.URL.Query()["protocol_version"]
	if len(versions) != 1 || versions[0] != strconv.Itoa(b.ScheduleProtocolVersion) {
		return 0, nil, b.Err(422, "scheduling protocol mismatch: Server requires protocol_version="+strconv.Itoa(b.ScheduleProtocolVersion)+"; upgrade Server and Dispatcher together")
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return 0, nil, b.Err(422, "invalid scheduling offset")
		}
		offset = n
	}
	p, err := t.ScheduleInput(r.PathValue("pid"), offset, r.URL.Query().Get("expected_version"))
	if err == nil && r.URL.Query().Has("namespace") {
		var namespace string
		namespace, err = executionNamespace(r)
		if err == nil {
			p.ExecutionChecks, err = t.ScheduleExecutionChecks(p.Project.ID, namespace, p.Steps)
		}
		if err == nil && offset == 0 {
			// Control admission shares the scheduling snapshot. Return it once,
			// alongside Execute checks, instead of adding a round trip per role.
			for _, kind := range []string{"reason", "curate"} {
				key := p.RetryKey
				if kind == "curate" {
					if !p.CurationNeeded {
						continue
					}
					key = p.CurationRetryKey
				}
				check, checkErr := t.CheckExecutions(b.ExecutionCheckQuery{ProjectID: p.Project.ID, Namespace: namespace, Generation: p.Project.Generation, Kind: kind, RetryKey: key})
				if checkErr != nil {
					return 0, nil, checkErr
				}
				p.ExecutionChecks[kind+":"] = check
			}
		}
	}
	return 200, p, err
}

// HTTP supplies project identity and the lease; the use case freezes its input.
func (s *Server) prepareExecution(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	var template b.Execution
	if err := decodeFields(q, &template); err != nil {
		return 0, nil, b.Err(422, "invalid execution template")
	}
	template.ProjectID = r.PathValue("pid")
	prepared, err := execution.PrepareRun(t, template, decisionFence(r))
	status := http.StatusOK
	if prepared.Created {
		status = http.StatusCreated
	}
	return status, prepared.Execution, err
}

func (s *Server) snapshotRead(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	var read protocol.GraphRequest
	if err := decodeStrictFields(q, &read); err != nil || read.Op != "read_snapshot" {
		return 0, nil, b.Err(422, "expected a read_snapshot request")
	}
	if err := protocol.ValidateGraphRequest(protocol.Job{}, read); err != nil {
		return 0, nil, b.Err(422, err.Error())
	}
	e, err := t.Execution(r.PathValue("pid"), r.PathValue("rid"))
	if err != nil {
		return 0, nil, err
	}
	if decisionFence(r) != e.Fence() {
		return 0, nil, b.Err(403, "Snapshot read requires its execution lease")
	}
	current, err := t.Load(e.ProjectID)
	if err != nil {
		return 0, nil, err
	}
	if err = t.CheckExecutionRead(current, decisionFence(r)); err != nil {
		return 0, nil, err
	}
	var j protocol.Job
	if err = json.Unmarshal(e.Job, &j); err != nil {
		return 0, nil, err
	}
	if j.InputSnapshot == nil {
		return 0, nil, b.Err(409, "Execution uses a legacy inline input")
	}
	state, err := t.ReadInputSnapshot(e.ProjectID, j.InputSnapshot.ID)
	if err != nil {
		return 0, nil, err
	}
	page, err := protocol.GraphPage(state, read)
	if err != nil {
		var failure *protocol.RPCError
		if errors.As(err, &failure) {
			return 0, nil, b.CodedErr(http.StatusUnprocessableEntity, failure.Code, failure.Detail)
		}
		return 0, nil, b.Err(422, err.Error())
	}
	return 200, page, nil
}

// Bind live execution reads to the registered input and current project round
// before either endpoint selects updates or retained process traces.
func executionReadState(t *b.Tx, e b.Execution, r *http.Request, read protocol.GraphRequest, roundError string) (protocol.Job, b.State, error) {
	var job protocol.Job
	var current b.State
	if err := json.Unmarshal(e.Job, &job); err != nil {
		return job, current, err
	}
	if err := protocol.ValidateGraphRequest(job, read); err != nil {
		return job, current, b.Err(422, err.Error())
	}
	current, err := t.State(e.ProjectID)
	if err != nil {
		return job, current, err
	}
	if err = t.CheckExecutionRead(current.Graph, decisionFence(r)); err != nil {
		return job, current, err
	}
	if job.Graph.Project.ID != e.ProjectID || job.Graph.Project.Generation != current.Graph.Project.Generation || job.RunID != e.ID || job.Kind != e.Kind {
		return job, current, b.Err(409, roundError)
	}
	return job, current, nil
}
