package sqlstore

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
		{"future", "(1,'unversioned-baseline'),(2,'pwnmesh-namespace'),(3,'future')", "unsupported PwnMesh schema version"},
		{"missing_baseline", "(2,'pwnmesh-namespace')", "inconsistent PwnMesh migration history"},
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

func assertStoreRejectedUnchanged(t *testing.T, path, message string) {
	t.Helper()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if store != nil {
		store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("unsupported store was not rejected: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected store changed database bytes: %v", err)
	}
}

func TestStoreRejectsUnversionedDataWithoutMutation(t *testing.T) {
	for _, seed := range []string{
		`CREATE TABLE projects(id TEXT PRIMARY KEY,bootstrap_mode TEXT NOT NULL); INSERT INTO projects VALUES('retained','disabled');`,
		`CREATE TABLE sqlitearchive(id INTEGER); INSERT INTO sqlitearchive VALUES(1);`,
		migrationLedgerSchema,
	} {
		t.Run(seed[:12], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unsupported.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(seed); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			message := "unversioned databases are unsupported"
			if seed == migrationLedgerSchema {
				message = "missing baseline"
			}
			assertStoreRejectedUnchanged(t, path, message)
		})
	}
}
