//go:build linux

package integration

import (
	"context"
	"testing"
	"time"

	"pwnmesh/internal/config"
	"pwnmesh/internal/docker"
	"pwnmesh/internal/worker"
)

type businessRetryDockerRunner struct {
	*docker.Client
	t       *testing.T
	started time.Time
}

func (r *businessRetryDockerRunner) Run(ctx context.Context, backend config.Worker, job worker.Job) (worker.Result, error) {
	if job.Kind == "explore" && job.PreviousRunID != "" {
		r.t.Logf("phase=retry_execute_started elapsed=%.3fs run=%s previous=%s model=local-scripted", time.Since(r.started).Seconds(), job.RunID, job.PreviousRunID)
	}
	result, err := r.Client.Run(ctx, backend, job)
	if job.Kind == "explore" && job.PreviousRunID == "" {
		r.t.Logf("phase=first_execute_returned elapsed=%.3fs status=%s failure=%s model=local-scripted", time.Since(r.started).Seconds(), result.Status, result.FailureKind)
	}
	return result, err
}
