//go:build linux

package worker

import (
	"pwnmesh/internal/config"
	"pwnmesh/internal/provider"
)

func modelForJob(j Job) (*provider.Anthropic, error) {
	p, err := provider.FromEnvironment(config.Getenv, j.Budget.ReasoningEffort)
	if err != nil {
		return nil, err
	}
	p.SessionID = j.RunID
	return p, nil
}
