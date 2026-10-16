package server

import (
	"encoding/json"
	"errors"
	"net/http"
	b "pwnmesh/internal/board"
	"pwnmesh/internal/execution"
	"strings"
)

func (s *Server) registerExecutionRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /projects/{pid}/scheduling", s.wrap(s.schedulingInput))
	m.HandleFunc("POST /projects/{pid}/executions/prepare", s.wrap(s.prepareExecution))
	m.HandleFunc("POST /projects/{pid}/executions/{rid}/input/read", s.readOnly(s.snapshotRead))
	m.HandleFunc("POST /projects/{pid}/executions/{rid}/updates", s.readOnly(s.executionUpdates))
	m.HandleFunc("POST /projects/{pid}/executions/{rid}/traces/read", s.readOnly(s.executionTraces))
	m.HandleFunc("GET /executions/pending", s.readOnly(s.pendingExecutions))
	m.HandleFunc("GET /projects/{pid}/executions/check", s.wrap(s.executionCheck))
	m.HandleFunc("GET /projects/{pid}/executions/{rid}", s.readOnly(s.executionDetail))
	m.HandleFunc("GET /projects/{pid}/executions/{rid}/identity", func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("execution_identity", "true")
		s.readOnly(s.executionDetail)(w, r)
	})
	for _, op := range []string{"status", "resume", "apply", "retry"} {
		m.HandleFunc("POST /projects/{pid}/executions/{rid}/"+op, func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("execution_op", op)
			s.wrap(s.executionAction)(w, r)
		})
	}
}
func decodeFields(q *request, v any) error {
	raw, err := json.Marshal(q.fields)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func (s *Server) executionAction(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	project, run, fence := r.PathValue("pid"), r.PathValue("rid"), decisionFence(r)
	switch r.PathValue("execution_op") {
	case "retry":
		// Reject an execution caller before decoding management-only fields.
		if fence.Run != "" {
			return 0, nil, b.Err(403, "retry authorization is a project-management operation")
		}
		automatic := false
		if value, exists := q.fields["automatic"]; exists {
			var ok bool
			automatic, ok = value.(bool)
			if !ok {
				return 0, nil, b.Err(422, "automatic must be a boolean")
			}
		}
		err := execution.Retry(t, project, run, fence, automatic)
		return http.StatusOK, map[string]string{"previous_run_id": run}, err
	case "resume":
		saved, err := execution.Resume(t, project, run, fence)
		return http.StatusOK, saved, err
	case "apply":
		status, err := execution.Apply(t, project, run, fence)
		return http.StatusOK, map[string]string{"status": status}, err
	default:
		status, valid := q.fields["status"].(string)
		status = strings.TrimSpace(status)
		var result json.RawMessage
		if value, exists := q.fields["result"]; exists {
			var err error
			result, err = json.Marshal(value)
			if err != nil {
				return 0, nil, err
			}
		}
		saved, err := execution.ChangeStatus(t, project, run, fence, status, result)
		// Preserve authority checks before reporting malformed status fields.
		var api *b.APIError
		if (!valid || status == "") && errors.As(err, &api) && api.Status == http.StatusUnprocessableEntity {
			return 0, nil, invalidField("status", "must be a non-empty string")
		}
		return http.StatusOK, saved, err
	}
}
