package worker

import "pwnmesh/internal/protocol"

func controlJob(j Job) bool            { return protocol.ControlJob(j) }
func orchestrationJob(j Job) bool      { return protocol.OrchestrationJob(j) }
func graphActions(j Job) []string      { return protocol.GraphActions(j) }
func validateCuratorInput(j Job) error { return protocol.ValidateCuratorInput(j) }
