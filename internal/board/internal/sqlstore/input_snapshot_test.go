package sqlstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"pwnmesh/internal/board/model"
)

type snapshotReadQueries struct {
	Queries
	chunkReads, largestPage int
}

func (q *snapshotReadQueries) Query(query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "pwnmesh_snapshot_chunks") {
		q.chunkReads++
		for _, arg := range args {
			if raw, ok := arg.(string); ok {
				var ids []string
				if json.Unmarshal([]byte(raw), &ids) == nil {
					q.largestPage = max(q.largestPage, len(ids))
				}
			}
		}
	}
	return q.Queries.Query(query, args...)
}

func (q *snapshotReadQueries) QueryRow(query string, args ...any) *sql.Row {
	if strings.Contains(query, "pwnmesh_snapshot_chunks") {
		q.chunkReads++
	}
	return q.Queries.QueryRow(query, args...)
}

func TestSnapshotChunkReadsAreBoundedAndKeepManifestOrder(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "batched-snapshots.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	// Repeated, nonadjacent chunks must survive batching with their exact order.
	// A short final chunk exercises the different terminal-size rule.
	chunks := [][]byte{bytes.Repeat([]byte{0, 1, 255, 2}, 256), bytes.Repeat([]byte{9, 8}, 512), []byte("tail")}
	manifest := snapshotManifest{}
	var original []byte
	if err := store.Do(ctx, func(q Queries) error {
		for _, project := range []string{"project", "other"} {
			if err := SaveGraph(q, model.Graph{Project: model.Project{ID: project, Title: project, Status: "active", CreatedAt: "now"}}); err != nil {
				return err
			}
		}
		for n := range 258 {
			chunk := chunks[n%2]
			if n == 257 {
				chunk = chunks[2]
			}
			digest := sha256.Sum256(chunk)
			id := hex.EncodeToString(digest[:])
			manifest.Chunks = append(manifest.Chunks, id)
			original = append(original, chunk...)
			if _, err := q.Exec("INSERT OR IGNORE INTO pwnmesh_snapshot_chunks(project_id,sha256,data) VALUES(?,?,?),(?,?,?)", "project", id, chunk, "other", id, []byte("foreign bytes")); err != nil {
				return err
			}
		}
		manifest.Bytes = len(original)
		encoded, err := json.Marshal(manifest)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(original)
		_, err = q.Exec("INSERT INTO pwnmesh_input_snapshots(project_id,id,metadata,state) VALUES(?,?,?,?)", "project", hex.EncodeToString(digest[:]), []byte(`{}`), append([]byte(snapshotChunkPrefix), encoded...))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(original)
	id := hex.EncodeToString(digest[:])
	if err := store.View(ctx, func(q Queries) error {
		counted := &snapshotReadQueries{Queries: q}
		got, err := ReadInputSnapshot(counted, "project", id)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, original) {
			t.Fatal("chunk order, duplicates, binary bytes or project scope changed")
		}
		if counted.chunkReads != 3 {
			t.Fatalf("258 manifest entries require 3 bounded chunk reads, got %d", counted.chunkReads)
		}
		if counted.largestPage > 128 {
			t.Fatalf("chunk read exceeded its bounded page: %d", counted.largestPage)
		}
		// The next decode must retain this WAL snapshot across a concurrent write.
		if err := store.Do(ctx, func(writer Queries) error {
			_, err := writer.Exec("UPDATE pwnmesh_snapshot_chunks SET data='changed' WHERE project_id='project'")
			return err
		}); err != nil {
			return err
		}
		got, err = ReadInputSnapshot(q, "project", id)
		if err == nil && !bytes.Equal(got, original) {
			t.Fatal("chunk pages escaped the caller's snapshot")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.View(ctx, func(q Queries) error {
		if _, err := ReadInputSnapshot(q, "project", id); err == nil {
			t.Fatal("fresh snapshot accepted changed chunk bytes")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotChunkPagesRejectCorruptStorage(t *testing.T) {
	for _, fault := range []string{"missing", "oversized", "empty", "short nonterminal", "changed bytes", "invalid identity", "invalid bounds", "wrong total", "wrong snapshot digest"} {
		t.Run(fault, func(t *testing.T) {
			store, err := Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.Do(context.Background(), func(q Queries) error {
				if err := SaveGraph(q, model.Graph{Project: model.Project{ID: "project", Title: "Project", Status: "active", CreatedAt: "now"}}); err != nil {
					return err
				}
				chunks := [][]byte{bytes.Repeat([]byte("a"), snapshotChunkMin), []byte("end")}
				original := append(append([]byte(nil), chunks[0]...), chunks[1]...)
				snapshotDigest := sha256.Sum256(original)
				manifest := snapshotManifest{Bytes: len(original)}
				for n, chunk := range chunks {
					if n == 0 && fault == "short nonterminal" {
						chunk = chunk[:snapshotChunkMin-1]
					}
					if n == 0 && fault == "wrong snapshot digest" {
						chunk = bytes.Repeat([]byte("b"), snapshotChunkMin)
					}
					digest := sha256.Sum256(chunk)
					id := hex.EncodeToString(digest[:])
					manifest.Chunks = append(manifest.Chunks, id)
					if n == 0 {
						switch fault {
						case "missing":
							continue
						case "oversized":
							chunk = bytes.Repeat([]byte("a"), snapshotChunkMax+1)
						case "empty":
							chunk = []byte{}
						case "changed bytes":
							chunk = bytes.Repeat([]byte("b"), snapshotChunkMin)
						}
					}
					if _, err := q.Exec("INSERT INTO pwnmesh_snapshot_chunks(project_id,sha256,data) VALUES(?,?,?)", "project", id, chunk); err != nil {
						return err
					}
				}
				switch fault {
				case "invalid identity":
					manifest.Chunks[0] = "invalid"
				case "invalid bounds":
					manifest.Bytes = int(^uint(0) >> 1)
				case "wrong total":
					manifest.Bytes++
				}
				encoded, err := json.Marshal(manifest)
				if err != nil {
					return err
				}
				id := hex.EncodeToString(snapshotDigest[:])
				if _, err := q.Exec("INSERT INTO pwnmesh_input_snapshots(project_id,id,metadata,state) VALUES(?,?,?,?)", "project", id, []byte(`{}`), append([]byte(snapshotChunkPrefix), encoded...)); err != nil {
					return err
				}
				if _, err := ReadInputSnapshot(q, "project", id); err == nil {
					t.Fatalf("accepted %s snapshot storage", fault)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSnapshotStoragePreservesBytesAndIdentity(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "snapshots.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Do(ctx, func(q Queries) error {
		for _, id := range []string{"project", "other"} {
			if err := SaveGraph(q, model.Graph{Project: model.Project{ID: id, Title: id, Status: "active", CreatedAt: "now"}}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"inline", []byte(`{"observations":[],"retained":null}`)},
		{"chunked", bytes.Repeat([]byte("original observation\n"), 4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			digest := sha256.Sum256(tc.raw)
			id := hex.EncodeToString(digest[:])
			metadata := []byte(`{"version":1,"retained":"original metadata"}`)
			if err := store.Do(ctx, func(q Queries) error {
				if err := SaveInputSnapshot(q, "project", id, metadata, tc.raw); err != nil {
					return err
				}
				// An existing immutable identity wins over a repeated caller's bytes.
				return SaveInputSnapshot(q, "project", id, []byte(`{}`), []byte("replacement"))
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.View(ctx, func(q Queries) error {
				got, err := ReadInputSnapshot(q, "project", id)
				if err != nil {
					return err
				}
				if !bytes.Equal(got, tc.raw) {
					t.Fatal("stored input bytes changed")
				}
				gotMetadata, err := ReadInputSnapshotMetadata(q, "project", id)
				if err != nil {
					return err
				}
				if !bytes.Equal(gotMetadata, metadata) {
					t.Fatal("stored metadata changed")
				}
				if _, err := ReadInputSnapshot(q, "other", id); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("cross-project read = %v", err)
				}
				if _, err := ReadInputSnapshotMetadata(q, "other", id); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("cross-project metadata = %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSnapshotStorageKeepsOuterTransactionOwnership(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	rejected := errors.New("outer operation rejected")
	raw := bytes.Repeat([]byte("uncommitted observation\n"), 4096)
	digest := sha256.Sum256(raw)
	id := hex.EncodeToString(digest[:])
	if err := store.Do(ctx, func(q Queries) error {
		if err := SaveGraph(q, model.Graph{Project: model.Project{ID: "project", Title: "Project", Status: "active", CreatedAt: "now"}}); err != nil {
			return err
		}
		if err := SaveInputSnapshot(q, "project", id, []byte(`{}`), raw); err != nil {
			return err
		}
		if got, err := ReadInputSnapshot(q, "project", id); err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("same-transaction read = %q, %v", got, err)
		}
		return rejected
	}); !errors.Is(err, rejected) {
		t.Fatalf("transaction error = %v", err)
	}
	if err := store.View(ctx, func(q Queries) error {
		for _, table := range []string{"pwnmesh_input_snapshots", "pwnmesh_snapshot_chunks"} {
			var count int
			if err := q.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("storage committed %s independently: %d", table, count)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
