package sqlstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"pwnmesh/internal/board/model"
)

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
