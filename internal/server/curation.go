package server

import (
	"net/http"

	b "pwnmesh/internal/board"
)

func (s *Server) curate(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	input := leaseOperation(q, r)
	if q.err != nil {
		return 0, nil, q.err
	}
	project, err := t.CuratorLease(r.PathValue("pid"), input)
	return http.StatusOK, project, err
}

func (s *Server) curationReceipt(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	receipt, status, err := t.CurationExecutionReceipt(r.PathValue("pid"), decisionFence(r))
	// Recovery needs only acknowledgement. Repeating the complete submission
	// body could exceed the bounded bridge when its original response was lost.
	receipt.Result = nil
	return http.StatusOK, struct {
		b.StateActionResult
		ExecutionStatus string `json:"execution_status"`
	}{receipt, status}, err
}
