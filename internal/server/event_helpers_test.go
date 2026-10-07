package server

import (
	"net/http"
	"pwnmesh/internal/board"
)

func storedEvents(f *executionProtocolFixture) []board.StateEvent {
	f.t.Helper()
	var events []board.StateEvent
	f.request("GET", f.base()+"/state/events", nil, false, http.StatusOK, &events)
	return events
}
