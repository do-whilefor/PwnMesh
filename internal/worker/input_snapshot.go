package worker

import "pwnmesh/internal/protocol"

func validateSnapshotInput(j Job) error { return protocol.ValidateSnapshotInput(j) }
