//go:build linux

package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/process"
	"time"
)

// prepareSession validates immutable input before acquiring any process resources.
func prepareSession(parent context.Context, j *Job, o *Options) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if j.RunID == "" {
		return errors.New("job requires run_id")
	}
	if j.PreviousRunID == j.RunID || j.Graph.Project.ID == "" {
		return errors.New("job requires a project and a distinct previous_run_id")
	}
	if err := validateWorkerProtocol(*j); err != nil {
		return err
	}
	if j.Repair != nil {
		if j.Kind != "explore" {
			return errors.New("repair requires an orchestration execute job with evidence protocol 2")
		}
		if err := artifactcheck.Validate(*j.Repair); err != nil {
			return err
		}
	}
	if j.Kind == "curate" {
		if err := validateCuratorInput(*j); err != nil {
			return err
		}
	}
	if !controlJob(*j) && j.Intent == nil {
		return errors.New("job requires an intent")
	}
	if j.Budget.Timeout < 0 || j.Budget.ConcludeTimeout < 0 {
		return errors.New("task budgets must not be negative")
	}
	if !controlJob(*j) && j.Budget.ConcludeTimeout <= 0 {
		return errors.New("conclude timeout must be positive")
	}
	if o.RunDir == "" || j.Workspace == "" {
		return errors.New("job requires workspace and execution directory")
	}
	var err error
	o.RunDir, err = filepath.Abs(o.RunDir)
	if err != nil {
		return err
	}
	j.Workspace, err = filepath.Abs(j.Workspace)
	if err != nil {
		return err
	}
	if err = verifyInputFiles(parent, *j); err != nil {
		return err
	}
	return nil
}

// acquireSession owns the run lock, PID marker and cancellation watcher.
// Release observes cancellation before cancelling the private context; normal
// completion and infrastructure interruption must not leave a hard-stop marker.
func acquireSession(parent context.Context, runDir string) (context.Context, func(), error) {
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return nil, nil, err
	}
	unlock, err := process.Lock(runDir)
	if err != nil {
		return nil, nil, err
	}
	if err = process.CheckLaunch(runDir); err != nil {
		unlock()
		return nil, nil, err
	}
	if process.Cancelled(runDir) {
		unlock()
		return nil, nil, context.Canceled
	}
	if err = process.RegisterWorker(runDir); err != nil {
		unlock()
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	release := func() {
		if ctx.Err() != nil && !errors.Is(context.Cause(parent), ErrInterrupted) {
			_ = os.WriteFile(filepath.Join(runDir, "cancelled"), []byte("hard stop\n"), 0600)
		}
		cancel()
		process.KillGroups(runDir)
		_ = os.Remove(filepath.Join(runDir, "worker.pid"))
		unlock()
	}
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if process.Cancelled(runDir) {
					cancel()
					return
				}
			}
		}
	}()
	if process.Cancelled(runDir) {
		release()
		return nil, nil, context.Canceled
	}
	return ctx, release, nil
}
