package server

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"

	b "pwnmesh/internal/board"
)

// The curator owns a separate lease from planning. Its operation must never
// claim, refresh or release the reason lease on the same project.
func (s *Server) curate(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	worker, op := q.text("worker"), r.PathValue("op")
	trigger := ""
	if op == "claim" {
		trigger = q.text("trigger")
	}
	if q.err != nil {
		return 0, nil, q.err
	}
	project := r.PathValue("pid")
	fence := decisionFence(r)
	if fence.Run != "" && (fence.Run != worker || fence.Lease != "curate" || fence.Intent != "") {
		return 0, nil, b.Err(403, "Curator lease identity mismatch")
	}
	if op == "claim" || op == "heartbeat" {
		if err := guardClaim(t, project, worker, fence); err != nil {
			return 0, nil, err
		}
	}
	var err error
	switch op {
	case "claim":
		_, err = t.ClaimCurator(project, worker, trigger)
	case "heartbeat":
		err = t.HeartbeatCurator(project, worker)
		if err == nil {
			if _, present := q.fields["expected_version"]; present {
				expected := q.text("expected_version")
				if _, decodeErr := hex.DecodeString(expected); len(expected) != 64 || decodeErr != nil {
					q.invalid("expected_version", "must be a 64-character hexadecimal state version")
				}
				if q.err != nil {
					return 0, nil, q.err
				}
				state, stateErr := t.State(project)
				if stateErr != nil {
					return 0, nil, stateErr
				}
				err = t.CheckDecisionStateVersion(state, b.ExecutionFence{Run: worker, Lease: "curate"}, expected)
			}
		}
	case "release":
		err = t.ReleaseCurator(project, worker)
	default:
		return 0, nil, b.Err(404, "Not Found")
	}
	if err != nil {
		return 0, nil, err
	}
	graph, err := t.Load(project)
	return http.StatusOK, graph.Project, err
}

func (s *Server) curationReceipt(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	fence := decisionFence(r)
	if fence.Run == "" || fence.Lease != "curate" || fence.Intent != "" {
		return 0, nil, b.Err(403, "Curation receipt requires its curator identity")
	}
	var status string
	if err := t.QueryRow("SELECT status FROM xloom_executions WHERE project_id=? AND lease=? AND kind='curate' AND intent=''", r.PathValue("pid"), fence.Run).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return 0, nil, b.Err(403, "Curation receipt requires a registered curator execution")
	} else if err != nil {
		return 0, nil, err
	}
	// A persisted receipt remains readable after lease release, including when
	// the successful submission's response was lost before result application.
	receipt, err := t.CurationReceipt(r.PathValue("pid"), fence.Run)
	// The body contains complete findings and disputes for direct submission.
	// Recovery only needs an acknowledgement; repeating that body could exceed
	// the bounded graph bridge precisely when a lost response needs recovery.
	receipt.Result = nil
	return http.StatusOK, struct {
		b.StateActionResult
		ExecutionStatus string `json:"execution_status"`
	}{receipt, status}, err
}
