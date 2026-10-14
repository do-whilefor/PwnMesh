package board

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func createV1NamespaceFixture(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v1.db")
	raw, err := os.ReadFile("testdata/schema_v1.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(raw) + namespaceFixtureData + extra); err != nil {
		t.Fatal(err)
	}
	return path
}

// Sparse rowids, whitespace, binary bytes and old-name literals are deliberate.
// The fixture is populated with v1 SQL, independently of current schema constants.
const namespaceFixtureData = `
INSERT INTO projects(id,title,status,created_at) VALUES('proj_041','Retained xloom_ text','active','now');
INSERT INTO facts VALUES('f012','proj_041','unchanged xloom_ observation');
INSERT INTO hints VALUES('h004','proj_041','unchanged hint','user','now');
INSERT INTO intents(id,project_id,description,creator,created_at) VALUES('i008','proj_041','Task','planner','now');
INSERT INTO intent_sources VALUES('i008','proj_041','f012');
UPDATE counters SET value=99 WHERE name='project';
INSERT INTO scoped_counters VALUES('proj_041','fact',99),('proj_041','intent',99),('proj_041','hint',99);
UPDATE pwnmesh_installation SET identity='11111111111111111111111111111111';
INSERT INTO pwnmesh_dispatchers VALUES('ns','22222222222222222222222222222222','2999-01-01T00:00:00Z');
INSERT INTO xloom_revoked_runs VALUES('proj_041','retired@worker');
INSERT INTO xloom_state VALUES('proj_041',' { "marker":"xloom_state", "curation":{} } ',9,7);
INSERT INTO xloom_state_actions(rowid,project_id,idempotency_key,request,response) VALUES(71,'proj_041','receipt',' {"xloom_request":true} ',' {"ok":true} ');
INSERT INTO xloom_state_events VALUES('proj_041',9,' {"op":"retained","marker":"xloom_event"} ');
INSERT INTO xloom_executions(rowid,project_id,id,namespace,backend,kind,intent,lease,job,retry_key,status,result,created_at,updated_at,generation,metadata_version)
 VALUES(70,'proj_041','later','ns','go','explore','i008','runner@later',CAST(' {"marker":"xloom_job"} ' AS BLOB),'retry:later','running',X'00786C6F6F6D5FFF','now','now',2,1),
       (3,'proj_041','earlier','ns','go','reason','','planner@earlier',CAST(' {"marker":"xloom_job_earlier"} ' AS BLOB),'retry:earlier','prepared',NULL,'now','now',2,1);
INSERT INTO xloom_paused_executions VALUES('proj_041','later');
INSERT INTO xloom_decision_read_views VALUES('proj_041','later','frozen-version',X'00786C6F6F6D5FFF');
INSERT INTO xloom_input_snapshots VALUES('proj_041','snapshot',X'207B2276657273696F6E223A317D20',X'00786C6F6F6D5FFF');
INSERT INTO xloom_snapshot_chunks VALUES('proj_041','chunk',X'00786C6F6F6D5FFF');
INSERT INTO xloom_project_metadata VALUES('proj_041','ctf');
INSERT INTO xloom_project_rounds VALUES('proj_041',2,'now');
INSERT INTO xloom_round_history VALUES('proj_041',1,'then');
INSERT INTO xloom_round_entries(rowid,project_id,generation,kind,id,data,sha256)
 VALUES(99,'proj_041',1,'state','second',X'00786C6F6F6D5FFF','second-digest'),
       (11,'proj_041',1,'state','first',X'207B22786C6F6F6D223A317D20','first-digest');
INSERT INTO xloom_project_termination VALUES('proj_041','historical-termination');
INSERT INTO xloom_project_orchestration VALUES('proj_041',1,'curator@current','input','now','now');
INSERT INTO xloom_input_files VALUES('proj_041','file','evidence.bin',8,'file-digest','now',X'00786C6F6F6D5FFF');
INSERT INTO xloom_input_batches VALUES('proj_041','upload','request-digest',' {"marker":"xloom_response"} ');
INSERT INTO xloom_assets VALUES('proj_041','asset','host','example.test','');
INSERT INTO xloom_asset_anchors VALUES('proj_041',2,'fact','f012','asset');
`

func retainedNamespaceRows(t *testing.T, db *sql.DB, prefix string) map[string][][]any {
	t.Helper()
	tables := map[string]string{}
	for _, suffix := range namespaceTables {
		tables[suffix] = prefix + suffix
	}
	for _, table := range []string{"settings", "projects", "facts", "intents", "intent_sources", "hints", "counters", "scoped_counters", "pwnmesh_installation", "pwnmesh_dispatchers"} {
		tables[table] = table
	}
	out := make(map[string][][]any)
	for key, table := range tables {
		rows, err := db.Query("SELECT rowid,* FROM " + quoteSchemaName(table) + " ORDER BY rowid")
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
			out[key] = append(out[key], values)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	}
	return out
}

func canonicalNamespaceSchema(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query("SELECT type,name,tbl_name,COALESCE(sql,'') FROM sqlite_master ORDER BY type,name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var kind, name, table, definition string
		if err := rows.Scan(&kind, &name, &table, &definition); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(name, "xloom_") || strings.Contains(table, "xloom_") || strings.Contains(definition, "xloom_") {
			t.Fatalf("old namespace remains: %s %s", name, definition)
		}
		// ALTER TABLE adds identifier quotes; formatting is not schema behavior.
		definition = strings.ReplaceAll(definition, `"`, "")
		out[kind+":"+name] = table + ":" + strings.Join(strings.Fields(definition), " ")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPwnMeshNamespaceMigrationPreservesV1DataAndSchema(t *testing.T) {
	for _, obsoleteIndex := range []bool{false, true} {
		t.Run(fmt.Sprintf("obsolete_index_%t", obsoleteIndex), func(t *testing.T) {
			extra := ""
			if obsoleteIndex {
				extra = "CREATE INDEX xloom_execution_metadata_backfill ON xloom_executions(metadata_version) WHERE metadata_version=0;"
			}
			path := createV1NamespaceFixture(t, extra)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			before := retainedNamespaceRows(t, db, "xloom_")
			db.Close()
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			if after := retainedNamespaceRows(t, store.db, "pwnmesh_"); !reflect.DeepEqual(after, before) {
				for table, rows := range before {
					if !reflect.DeepEqual(rows, after[table]) {
						t.Errorf("migration changed rowids, bytes or values in %s: before=%v after=%v", table, rows, after[table])
					}
				}
				t.FailNow()
			}
			var baselineAt, namespaceAt string
			if err := store.db.QueryRow("SELECT applied_at FROM pwnmesh_schema_migrations WHERE version=1 AND name='unversioned-baseline'").Scan(&baselineAt); err != nil || baselineAt != "2026-10-14T00:00:00Z" {
				t.Fatalf("baseline ledger was rewritten: %q %v", baselineAt, err)
			}
			if err := store.db.QueryRow("SELECT applied_at FROM pwnmesh_schema_migrations WHERE version=2 AND name='pwnmesh-namespace'").Scan(&namespaceAt); err != nil || namespaceAt == "" {
				t.Fatalf("namespace migration was not recorded: %q %v", namespaceAt, err)
			}
			fresh, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
			if err != nil {
				t.Fatal(err)
			}
			wantSchema := canonicalNamespaceSchema(t, fresh.db)
			fresh.Close()
			if got := canonicalNamespaceSchema(t, store.db); !reflect.DeepEqual(got, wantSchema) {
				for name, want := range wantSchema {
					if got[name] != want {
						t.Errorf("schema differs for %s:\ngot=%s\nwant=%s", name, got[name], want)
					}
				}
				for name := range got {
					if _, exists := wantSchema[name]; !exists {
						t.Errorf("migrated schema retained extra object %s", name)
					}
				}
				t.FailNow()
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			var afterAt string
			if err := store.db.QueryRow("SELECT applied_at FROM pwnmesh_schema_migrations WHERE version=2").Scan(&afterAt); err != nil || afterAt != namespaceAt {
				t.Fatalf("reopen reapplied namespace migration: %q %v", afterAt, err)
			}
			if after := retainedNamespaceRows(t, store.db, "pwnmesh_"); !reflect.DeepEqual(after, before) {
				t.Fatal("reopen changed retained data")
			}
			checkNamespaceTriggerBehavior(t, store)
		})
	}
}

func checkNamespaceTriggerBehavior(t *testing.T, store *Store) {
	t.Helper()
	err := store.Do(context.Background(), func(tx *Tx) error {
		var previous int
		if err := tx.QueryRow("SELECT revision FROM pwnmesh_ui_execution_revisions WHERE project_id='proj_041'").Scan(&previous); err != nil {
			return err
		}
		assertRevision := func(wantDeleted int) {
			t.Helper()
			var current int
			if err := tx.QueryRow("SELECT revision FROM pwnmesh_ui_execution_revisions WHERE project_id='proj_041'").Scan(&current); err != nil || current != previous+1 {
				t.Fatalf("renamed UI trigger did not advance exactly once: %d -> %d %v", previous, current, err)
			}
			var changed, deleted int
			if err := tx.QueryRow("SELECT revision,deleted FROM pwnmesh_ui_execution_changes WHERE project_id='proj_041' AND execution_id='new'").Scan(&changed, &deleted); err != nil || changed != current || deleted != wantDeleted {
				t.Fatalf("renamed UI trigger recorded change (%d,%d), want (%d,%d): %v", changed, deleted, current, wantDeleted, err)
			}
			previous = current
		}
		putQueryExecution(t, tx, Execution{ProjectID: "proj_041", ID: "new", Status: "running"}, 2, "")
		assertRevision(0)
		if _, err := tx.Exec("INSERT INTO pwnmesh_decision_read_views VALUES('proj_041','new','v','{}'); UPDATE pwnmesh_executions SET status='succeeded' WHERE id='new'"); err != nil {
			return err
		}
		assertRevision(0)
		var count int
		if err := tx.QueryRow("SELECT COUNT(*) FROM pwnmesh_decision_read_views WHERE execution_id='new'").Scan(&count); err != nil || count != 0 {
			t.Fatalf("terminal trigger retained a pinned view: %d %v", count, err)
		}
		if _, err := tx.Exec("INSERT INTO pwnmesh_revoked_runs VALUES('proj_041','runner@later')"); err != nil {
			return err
		}
		if err := tx.QueryRow("SELECT COUNT(*) FROM pwnmesh_decision_read_views").Scan(&count); err != nil || count != 0 {
			t.Fatalf("revocation trigger retained a pinned view: %d %v", count, err)
		}
		if _, err := tx.Exec("DELETE FROM pwnmesh_executions WHERE id='new'"); err != nil {
			return err
		}
		assertRevision(1)
		if _, err := tx.Exec("INSERT INTO pwnmesh_decision_read_views VALUES('proj_041','later','v','{}'); DELETE FROM pwnmesh_executions WHERE id='later'; DELETE FROM pwnmesh_round_history; DELETE FROM pwnmesh_assets"); err != nil {
			return err
		}
		for _, table := range []string{"pwnmesh_paused_executions", "pwnmesh_decision_read_views", "pwnmesh_round_entries", "pwnmesh_asset_anchors"} {
			if err := tx.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("renamed FK did not cascade %s: %d %v", table, count, err)
			}
		}
		return checkStoreForeignKeys(tx.Tx)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPwnMeshNamespaceMigrationRejectsMixedAndRollsBack(t *testing.T) {
	for _, test := range []struct{ name, extra, message string }{
		{"collision", "CREATE TABLE pwnmesh_state(id TEXT);", "namespace"},
		{"case_collision", "CREATE TABLE PWNMESH_executions(id TEXT);", "namespace"},
		{"partial", "ALTER TABLE xloom_state RENAME TO pwnmesh_state;", "namespace"},
		{"missing_table", "DROP TABLE xloom_state;", "namespace"},
		{"false_v2", "INSERT INTO pwnmesh_schema_migrations VALUES(2,'pwnmesh-namespace','invalid');", "namespace"},
		{"old_metadata", "UPDATE xloom_executions SET metadata_version=0;", "unsupported execution metadata"},
		{"foreign_key", "INSERT INTO xloom_paused_executions VALUES('proj_041','missing');", "foreign keys"},
		{"ledger_failure", "CREATE TRIGGER reject_namespace BEFORE INSERT ON pwnmesh_schema_migrations WHEN NEW.version=2 BEGIN SELECT RAISE(ABORT,'namespace ledger rejected'); END;", "namespace ledger rejected"},
		{"maintenance_failure", "CREATE TRIGGER reject_seed BEFORE INSERT ON settings BEGIN SELECT RAISE(ABORT,'seed rejected'); END;", "seed rejected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := createV1NamespaceFixture(t, test.extra)
			assertStoreRejectedUnchanged(t, path, test.message)
		})
	}
}
