package sqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Sparse rowids, whitespace and binary bytes must survive current-store reopen.
const currentStoreData = `
INSERT INTO projects(id,title,created_at) VALUES('project','Retained project','now');
INSERT INTO pwnmesh_state VALUES('project',' { "curation":{} } ',9,7);
INSERT INTO pwnmesh_state_actions(rowid,project_id,idempotency_key,request,response) VALUES(71,'project','receipt',' {"request":true} ',' {"ok":true} ');
INSERT INTO pwnmesh_executions(rowid,project_id,id,namespace,backend,kind,intent,lease,job,retry_key,status,result,created_at,updated_at,generation,metadata_version)
 VALUES(70,'project','later','ns','go','explore','step','runner@later',CAST(' {"job":true} ' AS BLOB),'retry:later','running',X'0078FF','now','now',2,1),
       (3,'project','earlier','ns','go','reason','','planner@earlier',CAST(' {"earlier":true} ' AS BLOB),'retry:earlier','prepared',NULL,'now','now',2,1);
INSERT INTO pwnmesh_paused_executions VALUES('project','later');
INSERT INTO pwnmesh_decision_read_views VALUES('project','later','frozen-version',X'0078FF');
INSERT INTO pwnmesh_input_snapshots VALUES('project','snapshot',X'207B2276657273696F6E223A317D20',X'0078FF');
INSERT INTO pwnmesh_snapshot_chunks VALUES('project','chunk',X'0078FF');
INSERT INTO pwnmesh_round_history VALUES('project',1,'then');
INSERT INTO pwnmesh_round_entries(rowid,project_id,generation,kind,id,data,sha256)
 VALUES(99,'project',1,'state','second',X'0078FF','second-digest'),
       (11,'project',1,'state','first',X'207B2278223A317D20','first-digest');
INSERT INTO pwnmesh_assets VALUES('project','asset','host','example.test','');
INSERT INTO pwnmesh_asset_anchors VALUES('project',2,'fact','fact','asset');
`

func currentStoreRows(t *testing.T, db *sql.DB) map[string][][]any {
	t.Helper()
	tables := []string{"sqlite_master", "settings", "projects", "facts", "intents", "intent_sources", "hints", "counters", "scoped_counters"}
	for _, suffix := range namespaceTables {
		tables = append(tables, "pwnmesh_"+suffix)
	}
	out := make(map[string][][]any)
	for _, table := range tables {
		rows, err := db.Query("SELECT rowid,* FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			args := make([]any, len(columns))
			for n := range values {
				args[n] = &values[n]
			}
			if err := rows.Scan(args...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			out[table] = append(out[table], values)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	}
	return out
}

func TestCurrentStoreReopenPreservesSchemaAndRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var installation string
	var intentTimeout, reasonTimeout, counter, versions int
	if err := store.db.QueryRow(`SELECT intent_timeout,reason_timeout,
 (SELECT identity FROM pwnmesh_installation WHERE singleton=1),
 (SELECT value FROM counters WHERE name='project'),
 (SELECT COUNT(*) FROM pwnmesh_schema_migrations) FROM settings WHERE rowid=1`).Scan(&intentTimeout, &reasonTimeout, &installation, &counter, &versions); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if intentTimeout != 15 || reasonTimeout != 15 || len(installation) != 32 || counter != 0 || versions != len(storeSchemaHistory) {
		store.Close()
		t.Fatalf("initialization did not seed the current store: %d/%d %q %d/%d", intentTimeout, reasonTimeout, installation, counter, versions)
	}
	if _, err := store.db.Exec(currentStoreData); err != nil {
		store.Close()
		t.Fatal(err)
	}
	before := currentStoreRows(t, store.db)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if after := currentStoreRows(t, store.db); !reflect.DeepEqual(after, before) {
		t.Fatal("reopen changed schema, rowids, bytes, hashes, installation identity or ledger")
	}
	checkCurrentStoreTriggers(t, store)
}

func checkCurrentStoreTriggers(t *testing.T, store *Store) {
	t.Helper()
	err := store.Do(context.Background(), func(tx Queries) error {
		var previous int
		if err := tx.QueryRow("SELECT revision FROM pwnmesh_ui_execution_revisions WHERE project_id='project'").Scan(&previous); err != nil {
			return err
		}
		assertRevision := func(wantDeleted int) {
			t.Helper()
			var current, changed, deleted int
			if err := tx.QueryRow("SELECT revision FROM pwnmesh_ui_execution_revisions WHERE project_id='project'").Scan(&current); err != nil || current != previous+1 {
				t.Fatalf("UI trigger did not advance exactly once: %d -> %d %v", previous, current, err)
			}
			if err := tx.QueryRow("SELECT revision,deleted FROM pwnmesh_ui_execution_changes WHERE project_id='project' AND execution_id='new'").Scan(&changed, &deleted); err != nil || changed != current || deleted != wantDeleted {
				t.Fatalf("UI trigger recorded change (%d,%d), want (%d,%d): %v", changed, deleted, current, wantDeleted, err)
			}
			previous = current
		}
		if _, err := tx.Exec(`INSERT INTO pwnmesh_executions(project_id,id,namespace,backend,kind,intent,lease,job,retry_key,status,created_at,updated_at,generation,metadata_version)
 VALUES('project','new','ns','planner','reason','','planner@new','{"graph":{}}','reason:key','running','now','now',2,1)`); err != nil {
			return err
		}
		assertRevision(0)
		if _, err := tx.Exec("INSERT INTO pwnmesh_decision_read_views VALUES('project','new','v','{}'); UPDATE pwnmesh_executions SET status='succeeded' WHERE id='new'"); err != nil {
			return err
		}
		assertRevision(0)
		var count int
		if err := tx.QueryRow("SELECT COUNT(*) FROM pwnmesh_decision_read_views WHERE execution_id='new'").Scan(&count); err != nil || count != 0 {
			t.Fatalf("terminal trigger retained a pinned view: %d %v", count, err)
		}
		if _, err := tx.Exec("INSERT INTO pwnmesh_revoked_runs VALUES('project','runner@later')"); err != nil {
			return err
		}
		if err := tx.QueryRow("SELECT COUNT(*) FROM pwnmesh_decision_read_views").Scan(&count); err != nil || count != 0 {
			t.Fatalf("revocation trigger retained a pinned view: %d %v", count, err)
		}
		if _, err := tx.Exec("DELETE FROM pwnmesh_executions WHERE id='new'"); err != nil {
			return err
		}
		assertRevision(1)
		if _, err := tx.Exec("INSERT INTO pwnmesh_decision_read_views VALUES('project','later','v','{}'); DELETE FROM pwnmesh_executions WHERE id='later'; DELETE FROM pwnmesh_round_history; DELETE FROM pwnmesh_assets"); err != nil {
			return err
		}
		for _, table := range []string{"pwnmesh_paused_executions", "pwnmesh_decision_read_views", "pwnmesh_round_entries", "pwnmesh_asset_anchors"} {
			if err := tx.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("foreign key did not cascade %s: %d %v", table, count, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCurrentStoreRejectsUnsupportedSchemaWithoutMutation(t *testing.T) {
	for _, test := range []struct{ name, extra, message string }{
		{"old_namespace", "CREATE TABLE xloom_state(id TEXT);", "namespace"},
		{"case_collision", "CREATE TABLE XLOOM_state(id TEXT);", "namespace"},
		{"unexpected", "CREATE TABLE pwnmesh_old(id TEXT);", "namespace"},
		{"missing_table", "DROP TABLE pwnmesh_state;", "namespace"},
		{"missing_settings", "DROP TABLE settings;", "no such table: settings"},
		{"missing_records", "DROP TABLE pwnmesh_state_records;", "namespace"},
		{"missing_step_index", "DROP INDEX pwnmesh_state_steps_id;", "namespace"},
		{"missing_owner_index", "DROP INDEX pwnmesh_execution_write_owners;", "namespace"},
		{"missing_record_trigger", "DROP TRIGGER pwnmesh_schedule_invalidate_pwnmesh_state_records_update;", "namespace"},
		{"missing_trigger", "DROP TRIGGER pwnmesh_ui_execution_update;", "namespace"},
		{"old_metadata", "UPDATE pwnmesh_executions SET metadata_version=0;", "unsupported execution metadata"},
		{"future_metadata", "UPDATE pwnmesh_executions SET metadata_version=2;", "unsupported execution metadata"},
		{"maintenance_failure", "DELETE FROM settings; CREATE TRIGGER reject_seed BEFORE INSERT ON pwnmesh_installation BEGIN SELECT RAISE(ABORT,'seed rejected'); END;", "seed rejected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unsupported.db")
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(currentStoreData + test.extra); err != nil {
				store.Close()
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			assertStoreRejectedUnchanged(t, path, test.message)
		})
	}
}

func TestStoreRejectsUnsupportedHistoryBeforeApplicationSchema(t *testing.T) {
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
