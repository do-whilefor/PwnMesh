package worker

import (
	"pwnmesh/internal/board"
	"pwnmesh/internal/protocol"
)

func BindTraceRun(run board.TraceRun) (board.TraceRun, error) { return protocol.BindTraceRun(run) }
