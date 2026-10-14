//go:build linux

package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"pwnmesh/internal/protocol"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	board "pwnmesh/internal/board/model"
)

// configureGraphRequest owns graph versions, snapshot routing and receipts.
// Role tool assembly supplies the capabilities that may call this boundary.
func configureGraphRequest(j Job, o Options, runtime *roleRuntime) func(context.Context, GraphRequest) (string, error) {
	versioned := j.Kind == "reason" && j.Decision != nil || j.Kind == "curate"
	// Version tracking belongs to the runtime, not model-generated arguments.
	// Tool execution is serial, and the session saves it with the tool result.
	track := func(result string, err error) (string, error) {
		if err != nil || !versioned {
			return result, err
		}
		var receipt struct {
			StateVersion string `json:"state_version"`
		}
		if json.Unmarshal([]byte(result), &receipt) != nil || len(receipt.StateVersion) != 64 {
			return "", errors.New("versioned graph response has no valid state_version")
		}
		runtime.graph.Version = receipt.StateVersion
		return result, nil
	}
	return func(ctx context.Context, r GraphRequest) (string, error) {
		if runtime.graph.Conflict != "" && r.Op != "decision_receipt" && r.Op != "curate_receipt" {
			return "", errors.New(runtime.graph.Conflict)
		}
		// The curator's read_graph capability always means its frozen input.
		// Keep the tool contract while serving new jobs through snapshot RPC.
		if j.Kind == "curate" && j.InputSnapshot != nil && r.Op == "read_graph" {
			if r.ExpectedVersion != "" && r.ExpectedVersion != j.InputSnapshot.StateVersion {
				return "", errors.New("state_changed: read requires the immutable curation snapshot version")
			}
			r.Op = "read_snapshot"
		}
		frozen := r.Op == "read_snapshot"
		if frozen && j.InputSnapshot != nil {
			r.ExpectedVersion = j.InputSnapshot.StateVersion
		} else if versioned {
			if r.Op == "graph_action" {
				r.Action.ExpectedVersion = runtime.graph.Version
				if j.Kind == "curate" {
					r.Action.ExpectedVersion = j.CurationVersion()
				}
			} else if (r.ByteOffset != nil || (r.Section != "" && r.Section != "overview")) && (j.Kind != "curate" || r.ExpectedVersion == "") {
				r.ExpectedVersion = runtime.graph.Version
			}
		}
		id := make([]byte, 16)
		if _, err := rand.Read(id); err != nil {
			return "", err
		}
		r.RequestID = hex.EncodeToString(id)
		if err := protocol.ValidateGraphRequest(j, r); err != nil {
			return "", err
		}
		if j.Kind == "curate" && r.Op == "read_graph" {
			// The complete immutable input is already registered with this job.
			// Reading it locally avoids RPCs and cannot silently replace the
			// boundary used by the server's eventual compare-and-swap commit.
			page, err := protocol.GraphPage(*j.State, r)
			if err != nil {
				return "", err
			}
			raw, err := json.Marshal(page)
			return track(string(raw), err)
		}
		if !j.GraphRPC {
			return "", errors.New("live graph submission requires the dispatcher graph bridge")
		}
		if r.Op == "graph_action" {
			var err error
			r.Action, err = prepareEvidence(ctx, j, o.RunDir, r.Action)
			if err != nil {
				return "", err
			}
		}
		started := time.Now()
		raw, err := graphRPC(ctx, o.RunDir, o.Output, r)
		if r.Op == "curate_receipt" {
			var receipt board.StateActionResult
			if err == nil && json.Unmarshal([]byte(raw), &receipt) == nil && receipt.Committed {
				return track(raw, nil)
			}
			return raw, err
		}
		if j.Kind == "curate" && graphStateConflict(err) {
			runtime.graph.Conflict = err.Error()
		}
		if frozen {
			return raw, err
		}
		if batchDecision(j) && runtime.emit != nil {
			operation := decisionOperation{Op: r.Op, ElapsedMS: time.Since(started).Milliseconds(), Failed: err != nil, StateChanged: graphStateConflict(err)}
			if err == nil && (r.Op == "decision_commit" || r.Op == "decision_receipt") {
				var receipt board.DecisionReceipt
				if json.Unmarshal([]byte(raw), &receipt) == nil && receipt.Committed && len(receipt.StateVersion) == 64 {
					operation.Committed = true
					operation.Actions = receipt.ChangedActions
					if operation.Actions == 0 {
						// Older full receipts can still supply their action results.
						for _, result := range receipt.Results {
							if !result.Unchanged {
								operation.Actions++
							}
						}
					}
				}
			}
			runtime.emit(operation.event())
		}
		if runtime.decision != nil && graphStateConflict(err) {
			runtime.decision.invalidate()
		}
		if r.Op == "decision_preview" {
			return raw, err
		}
		if r.Op == "decision_receipt" {
			var receipt board.DecisionReceipt
			if err != nil || json.Unmarshal([]byte(raw), &receipt) != nil || !receipt.Committed {
				return raw, err
			}
		}
		raw, err = track(raw, err)
		if err == nil && r.Op == "read_graph" && !internalGraphRead(ctx) && runtime.decision != nil {
			runtime.decision.observeRead(r.Section, raw)
		}
		return raw, err
	}
}

// The bridge preserves ProtocolError's HTTP status and JSON detail in its
// error string. A user-supplied alias or validation message mentioning
// state_changed is not evidence that a transaction was rejected as stale.
func graphStateConflict(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	if strings.HasPrefix(message, "state_changed:") {
		return true
	}
	const prefix = "board HTTP 409: "
	if !strings.HasPrefix(message, prefix) {
		return false
	}
	var response struct {
		Detail string `json:"detail"`
	}
	return json.Unmarshal([]byte(strings.TrimPrefix(message, prefix)), &response) == nil && strings.HasPrefix(response.Detail, "state_changed:")
}

func graphRPC(parent context.Context, runDir string, output io.Writer, r GraphRequest) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(GraphRequestEvent{Type: "graph_request", Request: r})
	if err != nil {
		return "", err
	}
	if len(raw) > MaxGraphRPCBytes {
		return "", errors.New("graph request exceeds 128 KiB")
	}
	if _, err = output.Write(append(raw, '\n')); err != nil {
		return "", err
	}
	name := filepath.Join(runDir, "graph-response-"+r.RequestID+".json")
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		result, err := readGraphResponse(name, r.RequestID)
		if err == nil {
			return result, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("graph bridge response unavailable; inspect current graph before retrying an uncertain action: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

func readGraphResponse(name, id string) (string, error) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !stat.Mode().IsRegular() || stat.Size() > MaxGraphRPCBytes {
		return "", errors.New("invalid graph response file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxGraphRPCBytes+1))
	if err != nil {
		return "", err
	}
	if len(raw) > MaxGraphRPCBytes {
		return "", errors.New("graph response exceeds 128 KiB")
	}
	var response GraphResponse
	if err = json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("invalid graph response: %w", err)
	}
	if response.RequestID != id {
		return "", errors.New("graph response request_id mismatch")
	}
	if response.Error != "" {
		return "", errors.New(response.Error)
	}
	if len(response.Result) == 0 || !json.Valid(response.Result) {
		return "", errors.New("graph response has no valid result")
	}
	return strings.TrimSpace(string(response.Result)), nil
}
