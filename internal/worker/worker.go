//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"pwnmesh/internal/agent"
	"strings"
	"time"
)

// Options configures a Worker invocation. Mutable role and graph state belong
// to the private roleRuntime created for that invocation.
type Options struct {
	Provider            agent.Provider
	Tools               []agent.Tool
	RunDir              string
	Output              io.Writer
	Now                 func() time.Time
	SoftStop            <-chan struct{}
	ContextBytes        int
	ContextTokens       int
	ContextTargetTokens int
	graphDeadline       time.Time
	graphProvider       func(string) (agent.Provider, error)
}

func Execute(ctx context.Context, jobPath string, output io.Writer) error {
	if os.Getenv("PWNMESH_MODEL_BRIDGE") != "dispatcher-v1" {
		return errors.New("worker requires the dispatcher model bridge")
	}
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if value != "" && (strings.HasPrefix(upper, "ANTHROPIC_") || strings.HasPrefix(upper, "OPENAI_") || upper == "PWNMESH_MODEL_PROTOCOL") {
			return errors.New("worker must not receive provider environment settings")
		}
	}
	raw, err := os.ReadFile(jobPath)
	if err != nil {
		return err
	}
	var j Job
	if err = json.Unmarshal(raw, &j); err != nil {
		return err
	}
	_, err = Run(ctx, j, Options{RunDir: filepath.Dir(jobPath), Output: output})
	return err
}

func validateWorkerProtocol(j Job) error {
	if j.Graph.Project.OrchestrationVersion != 1 || j.ResultContractVersion != 2 || !j.GraphRPC {
		return errors.New("worker requires orchestration version 1 with graph RPC and result protocol 2")
	}
	if j.WorkerType != "" && j.WorkerType != "go" {
		return errors.New("worker requires the go backend")
	}
	if j.Kind != "reason" && j.Kind != "curate" && j.Kind != "explore" {
		return errors.New("invalid job kind")
	}
	if j.Kind == "reason" && (j.Decision == nil || j.Decision.Version != 2) {
		return errors.New("reason requires decision protocol 2")
	}
	return j.Budget.ValidateTimeouts()
}

// runSession keeps process ownership around recovery, execution and settlement.
// Each phase preserves the journal-before-side-effect ordering.
func runSession(parent context.Context, j Job, o Options) (Result, error) {
	if err := prepareSession(parent, &j, &o); err != nil {
		return Result{}, err
	}
	ctx, release, err := acquireSession(parent, o.RunDir)
	if err != nil {
		return Result{}, err
	}
	defer release()
	runtime, err := restoreSession(j, o)
	if err != nil {
		return Result{}, err
	}
	defer runtime.journal.Close()
	retained := runtime.state.Log
	if result, done, err := runtime.recover(ctx); done || err != nil {
		return result, err
	}
	return runtime.runAgent(ctx, retained)
}
