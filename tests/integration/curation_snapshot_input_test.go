//go:build linux

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
	"testing"
)

func curationSnapshotKey(id string) string { return "/server/input-snapshots/" + id + ".json" }

// The immutable inputs live on the server, outside the Docker workspace.
// Retain and verify their original bytes alongside the collected evidence.
func retainCurationSnapshots(ctx context.Context, store *board.Store, project string, files map[string][]byte, output string) error {
	snapshots := map[string][]byte{}
	if err := store.Do(ctx, func(tx *board.Tx) error {
		rows, err := tx.Query("SELECT job FROM xloom_executions WHERE project_id=? AND kind='curate'", project)
		if err != nil {
			return err
		}
		var refs []*board.InputSnapshot
		for rows.Next() {
			var raw []byte
			var job worker.Job
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return err
			}
			if err := json.Unmarshal(raw, &job); err != nil {
				rows.Close()
				return err
			}
			if job.InputSnapshot != nil {
				refs = append(refs, job.InputSnapshot)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, ref := range refs {
			var raw []byte
			if _, err := tx.ReadInputSnapshot(project, ref.ID); err != nil {
				return err
			}
			if err := tx.QueryRow("SELECT state FROM xloom_input_snapshots WHERE project_id=? AND id=?", project, ref.ID).Scan(&raw); err != nil {
				return err
			}
			snapshots[ref.ID] = raw
		}
		return nil
	}); err != nil {
		return err
	}
	for id, raw := range snapshots {
		files[curationSnapshotKey(id)] = raw
		if output != "" {
			dir := filepath.Join(output, "input-snapshots")
			if err := os.MkdirAll(dir, 0700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, id+".json"), raw, 0600); err != nil {
				return err
			}
		}
	}
	return nil
}

// Never hydrate a contradictory job or substitute the final live state for an
// omitted historical snapshot. Legacy inline jobs retain their old contract.
func retainedCurationInput(job worker.Job, files map[string][]byte) *board.State {
	if job.InputSnapshot == nil {
		return job.State
	}
	ref := job.InputSnapshot
	if job.State != nil || len(job.Graph.Facts)+len(job.Graph.Intents)+len(job.Graph.Hints) != 0 || ref.Version != 1 || ref.ProjectID != job.Graph.Project.ID || ref.Generation != job.Graph.Project.Generation {
		return nil
	}
	raw := files[curationSnapshotKey(ref.ID)]
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != ref.ID {
		return nil
	}
	var state board.State
	if json.Unmarshal(raw, &state) != nil || state.Graph.Project.ID != ref.ProjectID || state.Graph.Project.Generation != ref.Generation || state.Graph.Project.OrchestrationVersion != job.Graph.Project.OrchestrationVersion || state.Revision != ref.Revision || board.DecisionStateVersion(state) != ref.StateVersion {
		return nil
	}
	return &state
}

func TestCurationDeliveryChecksRetainedSnapshotIdentityAndBytes(t *testing.T) {
	fixture := func(t *testing.T) (board.State, map[string][]byte, string) {
		t.Helper()
		state, files := curationSupportDeliveryFixture(t)
		path := "/workspace/.pwnmesh/runs/curator-run/job.json"
		var job worker.Job
		if err := json.Unmarshal(files[path], &job); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(job.State)
		job.InputSnapshot = &board.InputSnapshot{Version: 1, ID: fmt.Sprintf("%x", sha256.Sum256(raw)), ProjectID: job.Graph.Project.ID, Generation: job.Graph.Project.Generation, Revision: job.State.Revision, StateVersion: board.DecisionStateVersion(*job.State)}
		job.InputView, _ = board.CurationContextView(*job.State, board.DefaultContextViewBytes)
		job.Graph, job.State = board.Graph{Project: job.Graph.Project}, nil
		files[curationSnapshotKey(job.InputSnapshot.ID)] = raw
		files[path], _ = json.Marshal(job)
		return state, files, path
	}
	state, files, _ := fixture(t)
	if failures := validateCurationSupportDelivery(state, files); len(failures) != 0 {
		t.Fatal(failures)
	}
	for _, mode := range []string{"missing", "changed_bytes", "mixed_inline", "wrong_generation", "wrong_version", "wrong_revision"} {
		t.Run(mode, func(t *testing.T) {
			state, files, path := fixture(t)
			var job worker.Job
			_ = json.Unmarshal(files[path], &job)
			key := curationSnapshotKey(job.InputSnapshot.ID)
			switch mode {
			case "missing":
				delete(files, key)
			case "changed_bytes":
				files[key] = append(files[key], ' ')
			case "mixed_inline":
				job.State = &state
			case "wrong_generation":
				job.InputSnapshot.Generation++
			case "wrong_version":
				job.InputSnapshot.StateVersion = "invalid"
			case "wrong_revision":
				job.InputSnapshot.Revision++
			}
			files[path], _ = json.Marshal(job)
			if failures := validateCurationSupportDelivery(state, files); len(failures) == 0 {
				t.Fatal("invalid retained snapshot accepted")
			}
		})
	}
}
