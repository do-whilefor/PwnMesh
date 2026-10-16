package server

import (
	"errors"
	"net/http"

	b "pwnmesh/internal/board"
	"pwnmesh/internal/protocol"
)

// Apply continuation boundaries before sending the current FGS over HTTP.
// This keeps one oversized Fact from preventing all live graph reads.
func (s *Server) graphRead(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	var read protocol.GraphRequest
	if err := decodeStrictFields(q, &read); err != nil || read.Op != "read_graph" {
		return 0, nil, b.Err(422, "expected a read_graph request")
	}
	if err := protocol.ValidateGraphRequest(protocol.Job{}, read); err != nil {
		return 0, nil, b.Err(422, err.Error())
	}
	state, err := t.State(r.PathValue("pid"))
	if err != nil {
		return 0, nil, err
	}
	if err = guard(t, state.Graph, r); err != nil {
		return 0, nil, err
	}
	// An unversioned overview explicitly refreshes a planner's read view.
	// Continuations and detail pages retain it even while other work publishes
	// evidence, so collecting a consistent input does not chase a moving graph.
	refresh := (read.Section == "" || read.Section == "overview") && read.ExpectedVersion == "" && read.ByteOffset == nil
	state, err = t.DecisionReadState(state, decisionFence(r), refresh)
	if err != nil {
		return 0, nil, err
	}
	page, err := protocol.GraphPage(state, read)
	if err != nil {
		var failure *protocol.RPCError
		if errors.As(err, &failure) {
			return 0, nil, b.CodedErr(failure.Status, failure.Code, failure.Detail)
		}
		return 0, nil, b.Err(http.StatusUnprocessableEntity, err.Error())
	}
	return http.StatusOK, page, nil
}
