package board

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestGrowingInputSnapshotsShareRetainedEvidence(t *testing.T) {
	f := newOrchestrationFixture(t)
	state := f.state()
	var refs []*InputSnapshot
	var originals [][]byte
	var fullBytes int
	f.do(func(tx *Tx) error {
		for n := range 24 {
			var excerpt strings.Builder
			for line := range 128 {
				fmt.Fprintf(&excerpt, "observation %d response line %d: original retained artifact bytes\n", n, line)
			}
			id := fmt.Sprintf("f%03d", n)
			state.Graph.Facts = append(state.Graph.Facts, Fact{ID: id, Description: "Retained observation " + id})
			state.FactRecords = append(state.FactRecords, FactRecord{ID: id, Description: "Retained observation " + id, Scope: "storage fixture", Status: "valid", Evidence: []EvidenceRef{{RunID: "worker@storage", Path: id + ".txt", Excerpt: excerpt.String()}}})
			state.Revision++
			ref, err := tx.FreezeInput(state)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(state)
			if err != nil {
				return err
			}
			refs, originals = append(refs, ref), append(originals, raw)
			fullBytes += len(raw)
		}
		var storedBytes int
		if err := tx.QueryRow("SELECT COALESCE(SUM(length(state)),0) FROM xloom_input_snapshots WHERE project_id='p'").Scan(&storedBytes); err != nil {
			return err
		}
		var chunkBytes int
		if err := tx.QueryRow("SELECT COALESCE(SUM(length(data)),0) FROM xloom_snapshot_chunks WHERE project_id='p'").Scan(&chunkBytes); err != nil {
			return err
		}
		if storedBytes+chunkBytes >= fullBytes/2 {
			t.Fatalf("snapshots still duplicate growing evidence: stored=%d full=%d", storedBytes+chunkBytes, fullBytes)
		}
		t.Logf("retained snapshot bytes=%d; full-copy bytes=%d", storedBytes+chunkBytes, fullBytes)
		return nil
	})
	reopenAssetFixture(t, f)
	f.do(func(tx *Tx) error {
		// Restart removes the current executions and projections, but archived
		// immutable input must remain independently readable.
		if _, err := tx.RestartProject("p", nil); err != nil {
			return err
		}
		for n, ref := range refs {
			got, err := tx.ReadInputSnapshot("p", ref.ID)
			if err != nil {
				return err
			}
			raw, _ := json.Marshal(got)
			if !bytes.Equal(raw, originals[n]) {
				t.Fatalf("snapshot %d changed after reopen/restart", n)
			}
			metadata, err := tx.InputSnapshotMetadata("p", ref.ID)
			if err != nil || !reflect.DeepEqual(metadata, ref) {
				t.Fatalf("snapshot metadata changed: %v", err)
			}
		}
		return nil
	})
}

func TestCurrentInlineInputSnapshotRemainsReadable(t *testing.T) {
	f := newOrchestrationFixture(t)
	state := f.state()
	raw, _ := json.Marshal(state)
	digest := sha256.Sum256(raw)
	id := hex.EncodeToString(digest[:])
	f.do(func(tx *Tx) error {
		if _, err := tx.Exec("INSERT INTO xloom_input_snapshots(project_id,id,metadata,state) VALUES(?,?,?,?)", "p", id, []byte(`{}`), raw); err != nil {
			return err
		}
		got, err := tx.ReadInputSnapshot("p", id)
		if err == nil && !reflect.DeepEqual(got, state) {
			t.Fatal("inline snapshot changed")
		}
		return err
	})
}

func TestChunkedInputSnapshotFailsClosedOnMissingOrCorruptStorage(t *testing.T) {
	for _, fault := range []string{"missing", "corrupt", "manifest", "length", "cross-project"} {
		t.Run(fault, func(t *testing.T) {
			f := newOrchestrationFixture(t)
			state := f.state()
			state.Graph.Facts = append(state.Graph.Facts, Fact{ID: "large", Description: strings.Repeat("unchanged evidence bytes ", 2048)})
			f.do(func(tx *Tx) error {
				ref, err := tx.FreezeInput(state)
				if err != nil {
					return err
				}
				var stored []byte
				if err := tx.QueryRow("SELECT state FROM xloom_input_snapshots WHERE project_id='p' AND id=?", ref.ID).Scan(&stored); err != nil {
					return err
				}
				var manifest snapshotManifest
				if !bytes.HasPrefix(stored, []byte(snapshotChunkPrefix)) || json.Unmarshal(stored[len(snapshotChunkPrefix):], &manifest) != nil || len(manifest.Chunks) == 0 {
					t.Fatal("fixture was not chunked")
				}
				switch fault {
				case "missing", "cross-project":
					if fault == "cross-project" {
						if err := tx.Save(Graph{Project: Project{ID: "other", Title: "Other", Status: "active", CreatedAt: tx.Now}}); err != nil {
							return err
						}
						if _, err := tx.Exec("INSERT INTO xloom_snapshot_chunks(project_id,sha256,data) SELECT 'other',sha256,data FROM xloom_snapshot_chunks WHERE project_id='p'"); err != nil {
							return err
						}
					}
					_, err = tx.Exec("DELETE FROM xloom_snapshot_chunks WHERE project_id='p' AND sha256=?", manifest.Chunks[0])
				case "corrupt":
					_, err = tx.Exec("UPDATE xloom_snapshot_chunks SET data='modified' WHERE project_id='p' AND sha256=?", manifest.Chunks[0])
				case "manifest", "length":
					if fault == "manifest" {
						manifest.Chunks[0] = "invalid"
					} else {
						manifest.Bytes = int(^uint(0) >> 1)
					}
					encoded, _ := json.Marshal(manifest)
					_, err = tx.Exec("UPDATE xloom_input_snapshots SET state=? WHERE project_id='p' AND id=?", append([]byte(snapshotChunkPrefix), encoded...), ref.ID)
				}
				if err != nil {
					return err
				}
				if _, err := tx.ReadInputSnapshot("p", ref.ID); err == nil {
					t.Fatalf("accepted %s storage instead of failing closed", fault)
				}
				return nil
			})
		})
	}
}

func TestFailedSnapshotManifestDoesNotLeaveOrphanChunks(t *testing.T) {
	f := newOrchestrationFixture(t)
	state := f.state()
	state.Graph.Facts = append(state.Graph.Facts, Fact{ID: "large", Description: strings.Repeat("retained artifact bytes ", 2048)})
	f.do(func(tx *Tx) error {
		if _, err := tx.Exec("CREATE TRIGGER reject_snapshot BEFORE INSERT ON xloom_input_snapshots BEGIN SELECT RAISE(FAIL,'synthetic snapshot failure'); END"); err != nil {
			return err
		}
		if _, err := tx.FreezeInput(state); err == nil || !strings.Contains(err.Error(), "synthetic snapshot failure") {
			t.Fatalf("expected manifest insertion failure: %v", err)
		}
		var count int
		if err := tx.QueryRow("SELECT COUNT(*) FROM xloom_snapshot_chunks").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("handled manifest failure retained orphaned chunk payloads")
		}
		return nil
	})
}
