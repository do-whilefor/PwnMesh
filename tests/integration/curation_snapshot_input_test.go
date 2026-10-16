//go:build linux

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
	"strings"
	"testing"
)

func diagnosticSnapshotFixture(t *testing.T, size int) (*board.Store, *board.InputSnapshot, []byte) {
	t.Helper()
	store, err := openFixtureStore(t, filepath.Join(t.TempDir(), "diagnostics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	state := board.State{Graph: board.Graph{Project: board.Project{ID: "diagnostics", Title: "Diagnostic collection", Status: "active", OrchestrationVersion: 1}, Facts: []board.Fact{{ID: "origin", Description: strings.Repeat("evidence", size)}}}}
	var ref *board.InputSnapshot
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		state.Graph.Project.CreatedAt = tx.Now
		if err := tx.Save(state.Graph); err != nil {
			return err
		}
		var err error
		ref, err = tx.FreezeInput(state)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(worker.Job{RunID: "first", Kind: "curate", Graph: board.Graph{Project: state.Graph.Project}, InputSnapshot: ref})
	if err := fixtureSQL(store).Do(context.Background(), func(tx *fixtureSQLTx) error {
		for _, run := range []struct{ id, namespace string }{{"first", "selected"}, {"second", "selected"}, {"other", "unselected"}} {
			if _, err := tx.Exec("INSERT INTO pwnmesh_executions(project_id,id,namespace,backend,kind,intent,lease,job,retry_key,status,created_at,updated_at) VALUES(?,?,?,?,?,'',?,?,?,'succeeded',?,?)", state.Graph.Project.ID, run.id, run.namespace, "fixture", "curate", "fixture@"+run.id, job, run.id, state.Graph.Project.CreatedAt, state.Graph.Project.CreatedAt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return store, ref, raw
}

func TestDiagnosticCollectionPreservesExecutionOrderAndSnapshotBytes(t *testing.T) {
	for _, size := range []int{1, 4000} {
		t.Run(fmt.Sprintf("evidence_%d", size), func(t *testing.T) {
			store, ref, want := diagnosticSnapshotFixture(t, size)
			runs, err := testExecutions(context.Background(), store, "selected")
			if err != nil || len(runs) != 2 || runs[0].ID != "first" || runs[1].ID != "second" || !bytes.Equal(runs[0].Job, runs[1].Job) {
				t.Fatalf("diagnostic execution selection or insertion order changed: %+v, %v", runs, err)
			}
			files, output := map[string][]byte{}, t.TempDir()
			if err := retainCurationSnapshots(context.Background(), store, ref.ProjectID, files, output); err != nil {
				t.Fatal(err)
			}
			got := files[curationSnapshotKey(ref.ID)]
			if !bytes.Equal(got, want) || fmt.Sprintf("%x", sha256.Sum256(got)) != ref.ID {
				t.Fatalf("diagnostic snapshot changed original bytes: got %d bytes, want %d", len(got), len(want))
			}
			saved, err := os.ReadFile(filepath.Join(output, "input-snapshots", ref.ID+".json"))
			if err != nil || !bytes.Equal(saved, want) {
				t.Fatalf("saved diagnostic snapshot differs: %v", err)
			}
		})
	}
}

func TestDiagnosticCollectionRejectsCorruptSnapshotChunks(t *testing.T) {
	store, ref, _ := diagnosticSnapshotFixture(t, 4000)
	if err := fixtureSQL(store).Do(context.Background(), func(tx *fixtureSQLTx) error {
		_, err := tx.Exec("UPDATE pwnmesh_snapshot_chunks SET data=X'00' WHERE project_id=?", ref.ProjectID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	if err := retainCurationSnapshots(context.Background(), store, ref.ProjectID, files, ""); err == nil || len(files) != 0 {
		t.Fatalf("corrupt retained bytes entered diagnostic evidence: files=%d error=%v", len(files), err)
	}
}

func curationSnapshotKey(id string) string { return "/server/input-snapshots/" + id + ".json" }

// The immutable inputs live on the server, outside the Docker workspace.
// Retain and verify their original bytes alongside the collected evidence.
func retainCurationSnapshots(ctx context.Context, store *board.Store, project string, files map[string][]byte, output string) error {
	snapshots := map[string][]byte{}
	if err := fixtureSQL(store).View(ctx, func(tx *fixtureSQLTx) error {
		rows, err := tx.Query("SELECT job FROM pwnmesh_executions WHERE project_id=? AND kind='curate'", project)
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
			if err := store.View(ctx, func(boardTx *board.Tx) error {
				_, err := boardTx.ReadInputSnapshot(project, ref.ID)
				return err
			}); err != nil {
				return err
			}
			raw, err := diagnosticSnapshotBytes(tx, project, ref.ID)
			if err != nil {
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

// Board validates the stored representation. Collection then copies its exact
// retained segments and checks their digest in this diagnostic read snapshot.
func diagnosticSnapshotBytes(tx *fixtureSQLTx, project, id string) ([]byte, error) {
	var raw []byte
	if err := tx.QueryRow("SELECT state FROM pwnmesh_input_snapshots WHERE project_id=? AND id=?", project, id).Scan(&raw); err != nil {
		return nil, err
	}
	const prefix = "PwnMesh snapshot chunks v1\n"
	if bytes.HasPrefix(raw, []byte(prefix)) {
		var manifest struct {
			Bytes  int      `json:"bytes"`
			Chunks []string `json:"chunks"`
		}
		if err := json.Unmarshal(raw[len(prefix):], &manifest); err != nil {
			return nil, err
		}
		var joined bytes.Buffer
		for _, chunkID := range manifest.Chunks {
			var chunk []byte
			if err := tx.QueryRow("SELECT data FROM pwnmesh_snapshot_chunks WHERE project_id=? AND sha256=?", project, chunkID).Scan(&chunk); err != nil {
				return nil, err
			}
			joined.Write(chunk)
		}
		if joined.Len() != manifest.Bytes {
			return nil, fmt.Errorf("diagnostic snapshot chunk length mismatch")
		}
		raw = joined.Bytes()
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != id {
		return nil, fmt.Errorf("diagnostic snapshot digest mismatch")
	}
	return raw, nil
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
