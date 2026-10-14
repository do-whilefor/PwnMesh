package board

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreMigrationBaselineAndReopenMaintenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "versioned.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()
	var appliedAt string
	if err := store.Do(context.Background(), func(tx *Tx) error {
		var version int
		var name string
		if err := tx.QueryRow("SELECT version,name,applied_at FROM pwnmesh_schema_migrations").Scan(&version, &name, &appliedAt); err != nil {
			return err
		}
		if version != 1 || name != "unversioned-baseline" || appliedAt == "" {
			t.Fatalf("unexpected baseline: %d %q %q", version, name, appliedAt)
		}
		// Retained/imported identities must still repair counters on later opens.
		if err := tx.Save(Graph{
			Project: Project{ID: "proj_041", Title: "Imported", Status: "active", CreatedAt: tx.Now},
			Facts:   []Fact{{ID: "f012", Description: "Imported observation"}},
		}); err != nil {
			return err
		}
		// An older writer can restore a row without its derived metadata after
		// version 1 was recorded. Schema versioning must not disable that repair.
		if _, err := tx.Exec(`INSERT INTO xloom_executions(` + executionColumns + `) VALUES('proj_041','restored','ns','planner','reason','','planner@restored','{"graph":{"project":{"generation":7}}}','reason:restored','succeeded',NULL,0,'now','now')`); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM counters WHERE name='project'; DELETE FROM scoped_counters;
DELETE FROM settings; DELETE FROM pwnmesh_installation;
DELETE FROM xloom_ui_execution_changes; DELETE FROM xloom_ui_execution_revisions;`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Do(context.Background(), func(tx *Tx) error {
		var count int
		var after string
		if err := tx.QueryRow("SELECT COUNT(*),MAX(applied_at) FROM pwnmesh_schema_migrations").Scan(&count, &after); err != nil {
			return err
		}
		if count != 1 || after != appliedAt {
			t.Fatalf("reopen rewrote migration history: count=%d before=%q after=%q", count, appliedAt, after)
		}
		if id, err := tx.Next("", "project"); err != nil || id != "proj_042" {
			t.Fatalf("versioned reopen did not repair project counter: %q %v", id, err)
		}
		if id, err := tx.Next("proj_041", "fact"); err != nil || id != "f013" {
			t.Fatalf("versioned reopen did not repair fact counter: %q %v", id, err)
		}
		var generation, metadataVersion int
		if err := tx.QueryRow("SELECT generation,metadata_version FROM xloom_executions WHERE id='restored'").Scan(&generation, &metadataVersion); err != nil || generation != 7 || metadataVersion != 1 {
			t.Fatalf("versioned reopen did not backfill restored execution metadata: generation=%d version=%d err=%v", generation, metadataVersion, err)
		}
		settings, err := tx.Settings()
		if err != nil || settings.IntentTimeout != 15 || settings.ReasonTimeout != 15 {
			t.Fatalf("versioned reopen did not restore default settings: %+v %v", settings, err)
		}
		var identity string
		if err := tx.QueryRow("SELECT identity FROM pwnmesh_installation WHERE singleton=1").Scan(&identity); err != nil || len(identity) != 32 {
			t.Fatalf("versioned reopen did not reseed missing installation: %q %v", identity, err)
		}
		if err := tx.QueryRow("SELECT COUNT(*) FROM xloom_ui_execution_changes c JOIN xloom_ui_execution_revisions r ON c.project_id=r.project_id WHERE c.execution_id='restored' AND c.revision<=r.revision").Scan(&count); err != nil || count != 1 {
			t.Fatalf("versioned reopen did not restore execution change projection: %d %v", count, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreMigrationDoesNotReplayBaselineOnVersionedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incomplete.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("DROP TABLE settings"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(path); err == nil {
		store.Close()
		t.Fatal("versioned schema damage silently replayed the baseline")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='settings'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed startup recreated a versioned schema object: %d %v", count, err)
	}
}

func TestStoreMigrationRejectsUnsupportedHistoryBeforeApplicationSchema(t *testing.T) {
	for _, test := range []struct {
		name, history, want string
	}{
		{"future", "(1,'unversioned-baseline'),(2,'future')", "unsupported PwnMesh schema version"},
		{"missing_baseline", "(2,'future')", "unsupported PwnMesh schema version"},
		{"renamed", "(1,'different-baseline')", "inconsistent PwnMesh migration history"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unsupported.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(migrationLedgerSchema + "INSERT INTO pwnmesh_schema_migrations(version,name) VALUES" + test.history); err != nil {
				t.Fatal(err)
			}
			var before string
			if err := db.QueryRow("SELECT json_group_array(json_object('version',version,'name',name,'applied_at',applied_at)) FROM pwnmesh_schema_migrations").Scan(&before); err != nil {
				t.Fatal(err)
			}
			store, err := Open(path)
			if store != nil {
				store.Close()
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsupported history was not rejected: %v", err)
			}
			var count int
			if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name!='pwnmesh_schema_migrations'").Scan(&count); err != nil || count != 0 {
				t.Fatalf("rejected history created application tables: %d %v", count, err)
			}
			var after string
			if err := db.QueryRow("SELECT json_group_array(json_object('version',version,'name',name,'applied_at',applied_at)) FROM pwnmesh_schema_migrations").Scan(&after); err != nil || before != after {
				t.Fatalf("rejected history changed ledger: before=%s after=%s err=%v", before, after, err)
			}
		})
	}
}
