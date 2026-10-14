package worker

import (
	board "pwnmesh/internal/board/model"
	"pwnmesh/internal/protocol"
)

func BindTraceRun(run board.TraceRun) (board.TraceRun, error) { return protocol.BindTraceRun(run) }
