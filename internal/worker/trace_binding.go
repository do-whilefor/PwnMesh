package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"

	"pwnmesh/internal/board"
)

// BindTraceRun runs on the server before a trace descriptor crosses the bridge.
// Execution.Job can have map-canonical field ordering after HTTP decoding;
// session identity instead hashes the decoded, typed Worker Job serialization.
func BindTraceRun(run board.TraceRun) (board.TraceRun, error) {
	var j Job
	if json.Unmarshal(run.RegisteredJob, &j) != nil || j.Kind != "explore" || j.RunID != run.RunID || j.Graph.Project.ID != run.ProjectID || j.Graph.Project.Generation != run.Generation || j.Workspace != run.Workspace || j.Intent == nil || j.Intent.ID != run.StepID {
		return board.TraceRun{}, errors.New("trace unavailable: registered execution identity mismatch")
	}
	// runSession normalizes this field before calling identityFor.
	var err error
	j.Workspace, err = filepath.Abs(j.Workspace)
	if err != nil {
		return board.TraceRun{}, err
	}
	run.Workspace = j.Workspace
	raw, err := json.Marshal(j)
	if err != nil {
		return board.TraceRun{}, err
	}
	digest := sha256.Sum256(raw)
	run.JobSHA256, run.RegisteredJob = hex.EncodeToString(digest[:]), nil
	return run, nil
}
