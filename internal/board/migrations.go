package board

import (
	"database/sql"
	"errors"
	"fmt"
)

// migrateStore owns the migration transaction; Open owns the connections.
// A crash cannot add bootstrap_enabled without mapping a legacy disabled
// bootstrap_mode, or leave only some counters repaired.
func migrateStore(db *sql.DB) error {
	migration, err := db.Begin()
	if err != nil {
		return err
	}
	defer migration.Rollback()
	if _, err = migration.Exec(schema + stateSchema + executionSchema + projectMetadataSchema + restartSchema + terminationSchema + orchestrationSchema + inputFileSchema + assetSchema + deploymentSchema); err != nil {
		return err
	}
	rows, err := migration.Query("PRAGMA table_info(projects)")
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, nn, pk int
		var name, typ string
		var def any
		if err = rows.Scan(&cid, &name, &typ, &nn, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !columns["bootstrap_enabled"] {
		_, err = migration.Exec("ALTER TABLE projects ADD COLUMN bootstrap_enabled INTEGER NOT NULL DEFAULT 1")
		if err == nil && columns["bootstrap_mode"] {
			_, err = migration.Exec("UPDATE projects SET bootstrap_enabled=CASE WHEN bootstrap_mode='disabled' THEN 0 ELSE 1 END")
		}
		if err != nil {
			return err
		}
	}
	// Older or partially restored Cairn databases may omit their counters. Never
	// let a newly allocated ID overwrite a project or reference an old Fact.
	if _, err = migration.Exec(`UPDATE counters SET value=MAX(value,COALESCE((SELECT MAX(CAST(SUBSTR(id,6) AS INTEGER)) FROM projects WHERE id GLOB 'proj_[0-9]*'),0)) WHERE name='project'`); err != nil {
		return err
	}
	for _, item := range []struct{ table, kind, prefix string }{{"facts", "fact", "f"}, {"intents", "intent", "i"}, {"hints", "hint", "h"}} {
		query := fmt.Sprintf(`INSERT INTO scoped_counters(project_id,kind,value) SELECT project_id,?,MAX(CAST(SUBSTR(id,2) AS INTEGER)) FROM %s WHERE id GLOB ? GROUP BY project_id ON CONFLICT(project_id,kind) DO UPDATE SET value=MAX(value,excluded.value)`, item.table)
		if _, err = migration.Exec(query, item.kind, item.prefix+"[0-9]*"); err != nil {
			return err
		}
	}
	if err = migrateExecutionMetadata(migration); err != nil {
		return err
	}
	return migration.Commit()
}

// Backfill once in Open's migration transaction. Read only one historical Job
// at a time and never change its bytes. Later opens and scheduling queries use
// the indexed metadata even if a completed Job is no longer decodable.
func migrateExecutionMetadata(tx *sql.Tx) error {
	rows, err := tx.Query("PRAGMA table_info(xloom_executions)")
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, nn, pk int
		var name, typ string
		var def any
		if err = rows.Scan(&cid, &name, &typ, &nn, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"generation", "INTEGER NOT NULL DEFAULT 0"}, {"input_revision", "INTEGER NOT NULL DEFAULT 0"},
		{"decision_revision", "INTEGER NOT NULL DEFAULT 0"}, {"state_version", "TEXT NOT NULL DEFAULT ''"},
		{"has_state", "INTEGER NOT NULL DEFAULT 0"}, {"fact_count", "INTEGER NOT NULL DEFAULT 0"},
		{"hint_count", "INTEGER NOT NULL DEFAULT 0"}, {"open_count", "INTEGER NOT NULL DEFAULT 0"},
		{"metadata_version", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if !columns[column.name] {
			if _, err = tx.Exec("ALTER TABLE xloom_executions ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec(`CREATE INDEX IF NOT EXISTS xloom_execution_metadata_backfill ON xloom_executions(metadata_version) WHERE metadata_version=0`); err != nil {
		return err
	}
	var after int64
	for {
		var id int64
		var raw []byte
		err = tx.QueryRow("SELECT rowid,job FROM xloom_executions WHERE metadata_version=0 AND rowid>? ORDER BY rowid LIMIT 1", after).Scan(&id, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		metadata, err := executionInputMetadata(raw)
		if err != nil {
			return fmt.Errorf("execution metadata migration row %d: %w", id, err)
		}
		args := append(executionMetadataValues(metadata), id)
		if _, err = tx.Exec(`UPDATE xloom_executions SET generation=?,input_revision=?,decision_revision=?,state_version=?,has_state=?,fact_count=?,hint_count=?,open_count=?,metadata_version=1 WHERE rowid=?`, args...); err != nil {
			return err
		}
		after = id
	}
	_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS xloom_execution_pending ON xloom_executions(namespace) WHERE ` + pendingExecutionSQL + `;
CREATE INDEX IF NOT EXISTS xloom_execution_task ON xloom_executions(namespace,project_id,kind,intent,status,created_at);
CREATE INDEX IF NOT EXISTS xloom_execution_retry ON xloom_executions(namespace,project_id,retry_key,generation);
CREATE INDEX IF NOT EXISTS xloom_execution_decision ON xloom_executions(namespace,project_id,generation,kind,status,created_at);
CREATE INDEX IF NOT EXISTS xloom_execution_version ON xloom_executions(namespace,project_id,kind,state_version);
CREATE INDEX IF NOT EXISTS xloom_execution_step ON xloom_executions(project_id,intent) WHERE kind!='reason';`)
	return err
}
