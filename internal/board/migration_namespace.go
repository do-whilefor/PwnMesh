package board

import (
	"database/sql"
	"fmt"
	"strings"
)

// The schema-v1 table inventory is frozen. Runtime SQL only uses pwnmesh_*;
// the previous names are confined to this one-time, transactional migration.
var namespaceTables = []string{
	"revoked_runs", "state", "state_actions", "state_events", "executions",
	"paused_executions", "input_snapshots", "snapshot_chunks", "decision_read_views",
	"project_metadata", "ui_execution_revisions", "ui_execution_changes",
	"project_rounds", "round_history", "round_entries", "project_termination",
	"project_orchestration", "input_files", "input_batches", "assets", "asset_anchors",
}

var namespaceObjects = []struct{ kind, name, table string }{
	{"index", "execution_pending", "executions"},
	{"index", "execution_task", "executions"},
	{"index", "execution_retry", "executions"},
	{"index", "execution_decision", "executions"},
	{"index", "execution_version", "executions"},
	{"index", "execution_step", "executions"},
	{"index", "ui_execution_changes_revision", "ui_execution_changes"},
	{"index", "asset_anchors_asset", "asset_anchors"},
	{"trigger", "decision_read_terminal", "executions"},
	{"trigger", "decision_read_revoked", "revoked_runs"},
	{"trigger", "ui_execution_insert", "executions"},
	{"trigger", "ui_execution_update", "executions"},
	{"trigger", "ui_execution_delete", "executions"},
}

type namespaceObject struct{ kind, name, table, definition string }

func readNamespaceObjects(tx *sql.Tx) (map[string]namespaceObject, error) {
	rows, err := tx.Query("SELECT type,name,tbl_name,COALESCE(sql,'') FROM sqlite_master WHERE name NOT GLOB 'sqlite_*'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := make(map[string]namespaceObject)
	for rows.Next() {
		var object namespaceObject
		if err := rows.Scan(&object.kind, &object.name, &object.table, &object.definition); err != nil {
			return nil, err
		}
		objects[strings.ToLower(object.name)] = object
	}
	return objects, rows.Err()
}

func checkNamespace(objects map[string]namespaceObject, prefix string) error {
	expected := map[string]namespaceObject{}
	for _, name := range []string{"pwnmesh_schema_migrations", "pwnmesh_installation", "pwnmesh_dispatchers"} {
		expected[name] = namespaceObject{kind: "table", table: name}
	}
	for _, suffix := range namespaceTables {
		name := prefix + suffix
		expected[name] = namespaceObject{kind: "table", table: name}
	}
	for _, object := range namespaceObjects {
		expected[prefix+object.name] = namespaceObject{kind: object.kind, table: prefix + object.table}
	}
	for name, want := range expected {
		got, ok := objects[name]
		if !ok || got.kind != want.kind || got.table != want.table {
			return fmt.Errorf("inconsistent schema namespace: expected %s %s on %s", want.kind, name, want.table)
		}
	}
	for name, object := range objects {
		// Earlier adopted-v1 databases retain this unused partial index.
		if prefix == "xloom_" && name == "xloom_execution_metadata_backfill" && object.kind == "index" && object.table == "xloom_executions" {
			continue
		}
		if strings.HasPrefix(name, "xloom_") || strings.HasPrefix(name, "pwnmesh_") {
			if _, ok := expected[name]; !ok {
				return fmt.Errorf("mixed or unexpected schema namespace object %s", name)
			}
		}
	}
	return nil
}

func validatePwnMeshNamespace(tx *sql.Tx) error {
	objects, err := readNamespaceObjects(tx)
	if err != nil {
		return err
	}
	return checkNamespace(objects, "pwnmesh_")
}

func migratePwnMeshNamespace(tx *sql.Tx) error {
	objects, err := readNamespaceObjects(tx)
	if err != nil {
		return err
	}
	if err := checkNamespace(objects, "xloom_"); err != nil {
		return err
	}
	if err := validateExecutionMetadata(tx, "xloom_executions"); err != nil {
		return err
	}
	if err := checkStoreForeignKeys(tx); err != nil {
		return err
	}
	// Drop triggers first: SQLite table renaming can rewrite an UPSERT's
	// excluded.revision alias to the target table and change cursor behavior.
	for _, spec := range namespaceObjects {
		if spec.kind == "trigger" {
			if _, err := tx.Exec("DROP TRIGGER " + quoteSchemaName("xloom_"+spec.name)); err != nil {
				return err
			}
		}
	}
	// SQLite updates FK/index references during table rename. Never copy rows
	// or re-encode retained JSON; the original rowids and record bytes survive.
	if _, err := tx.Exec("PRAGMA legacy_alter_table=OFF"); err != nil {
		return err
	}
	for _, suffix := range namespaceTables {
		if _, err := tx.Exec("ALTER TABLE " + quoteSchemaName("xloom_"+suffix) + " RENAME TO " + quoteSchemaName("pwnmesh_"+suffix)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DROP INDEX IF EXISTS xloom_execution_metadata_backfill"); err != nil {
		return err
	}
	objects, err = readNamespaceObjects(tx)
	if err != nil {
		return err
	}
	// SQLite rewrites referenced table names but retains explicit index names.
	// Replace only each declared name in that rewritten SQL, preserving its body.
	for _, spec := range namespaceObjects {
		if spec.kind != "index" {
			continue
		}
		oldName, newName := "xloom_"+spec.name, "pwnmesh_"+spec.name
		object := objects[oldName]
		if !strings.Contains(object.definition, oldName) {
			return fmt.Errorf("missing schema definition for %s", oldName)
		}
		if _, err := tx.Exec("DROP " + strings.ToUpper(spec.kind) + " " + quoteSchemaName(oldName)); err != nil {
			return err
		}
		if _, err := tx.Exec(strings.Replace(object.definition, oldName, newName, 1)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(decisionReadTriggerSchema + executionChangeTriggerSchema); err != nil {
		return err
	}
	if err := validatePwnMeshNamespace(tx); err != nil {
		return err
	}
	return checkStoreForeignKeys(tx)
}

func checkStoreForeignKeys(tx *sql.Tx) error {
	rows, err := tx.Query("PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("schema namespace migration requires intact foreign keys")
	}
	return rows.Err()
}

func quoteSchemaName(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
