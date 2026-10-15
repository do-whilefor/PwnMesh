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
		{"retired_v1", "(1,'unversioned-baseline')", "unsupported PwnMesh schema version 1"},
		{"retired_v2", "(1,'unversioned-baseline'),(2,'pwnmesh-namespace')", "unsupported PwnMesh schema version 2"},
		{"retired_v3", "(1,'unversioned-baseline'),(2,'pwnmesh-namespace'),(3,'incremental-state-storage')", "unsupported PwnMesh schema version 3"},
		{"future", "(1,'unversioned-baseline'),(2,'pwnmesh-namespace'),(5,'future')", "unsupported PwnMesh schema version"},
		{"missing_baseline", "(2,'pwnmesh-namespace')", "inconsistent PwnMesh migration history"},
		{"renamed", "(1,'different-baseline')", "inconsistent PwnMesh migration history"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unsupported.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(migrationLedgerSchema + "INSERT INTO pwnmesh_schema_migrations(version,name) VALUES" + test.history + `;
CREATE TABLE retained_bytes(id INTEGER PRIMARY KEY,data BLOB);
INSERT INTO retained_bytes VALUES(71,X'0078FF');`); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			assertStoreRejectedUnchanged(t, path, test.want)
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
