package sqlstore

import (
	"database/sql"
	"fmt"
	"strings"
)

var namespaceTables = []string{
	"schema_migrations", "installation", "dispatchers",
	"revoked_runs", "state", "state_actions", "state_events", "executions",
	"paused_executions", "input_snapshots", "snapshot_chunks", "decision_read_views",
	"project_metadata", "ui_execution_revisions", "ui_execution_changes",
	"project_rounds", "round_history", "round_entries", "project_termination",
	"project_orchestration", "input_files", "input_batches", "assets", "asset_anchors",
	"fact_metadata", "schedule_cache", "schedule_steps", "state_records",
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

func validatePwnMeshNamespace(tx *sql.Tx) error {
	return validatePwnMeshNamespaceVersion(tx, len(storeSchemaHistory))
}

func validatePwnMeshNamespaceVersion(tx *sql.Tx, version int) error {
	type object struct{ kind, table string }
	expected := map[string]object{}
	for _, suffix := range namespaceTables {
		if version < 4 && suffix == "state_records" {
			continue
		}
		if version < 3 && (suffix == "fact_metadata" || suffix == "schedule_cache" || suffix == "schedule_steps") {
			continue
		}
		name := "pwnmesh_" + suffix
		expected[name] = object{kind: "table", table: name}
	}
	for _, spec := range namespaceObjects {
		expected["pwnmesh_"+spec.name] = object{kind: spec.kind, table: "pwnmesh_" + spec.table}
	}
	if version >= 3 {
		for _, spec := range scheduleInvalidationObjects() {
			if version < 4 && spec.table == "pwnmesh_state_records" {
				continue
			}
			expected["pwnmesh_"+spec.name] = object{kind: spec.kind, table: spec.table}
		}
	}
	if version >= 4 {
		expected["pwnmesh_state_steps_id"] = object{kind: "index", table: "pwnmesh_state_records"}
		expected["pwnmesh_execution_write_owners"] = object{kind: "index", table: "pwnmesh_executions"}
	}
	rows, err := tx.Query("SELECT type,name,tbl_name FROM sqlite_master WHERE name NOT GLOB 'sqlite_*'")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var got object
		if err := rows.Scan(&got.kind, &name, &got.table); err != nil {
			return err
		}
		name = strings.ToLower(name)
		if want, ok := expected[name]; ok {
			if got != want {
				return fmt.Errorf("inconsistent schema namespace: expected %s %s on %s", want.kind, name, want.table)
			}
			delete(expected, name)
		} else if strings.HasPrefix(name, "xloom_") || strings.HasPrefix(name, "pwnmesh_") {
			return fmt.Errorf("mixed or unexpected schema namespace object %s", name)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for name, want := range expected {
		return fmt.Errorf("inconsistent schema namespace: expected %s %s on %s", want.kind, name, want.table)
	}
	return nil
}
