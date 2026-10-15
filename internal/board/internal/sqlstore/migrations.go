package sqlstore

import (
	"database/sql"
	"errors"
	"fmt"
)

// Version 2 is the supported upgrade baseline; retired/unversioned stores are
// never adopted by replaying current DDL over their unknown schema.
var storeSchemaHistory = [...]string{"unversioned-baseline", "pwnmesh-namespace", "incremental-state-storage", "normalized-state-records"}

const migrationLedgerSchema = `CREATE TABLE IF NOT EXISTS pwnmesh_schema_migrations(
 version INTEGER PRIMARY KEY CHECK(version > 0),name TEXT NOT NULL,
 applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')));`

// Validate support before any writes. Empty stores initialize directly; version
// 2 moves Fact metadata into records; 3 normalizes the remaining collections.
// Current-version reopen preserves bytes.
func migrateStore(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var objects, ledgers int
	if err := tx.QueryRow(`SELECT COUNT(*),COALESCE(SUM(type='table' AND name='pwnmesh_schema_migrations'),0) FROM sqlite_master WHERE name NOT GLOB 'sqlite_*'`).Scan(&objects, &ledgers); err != nil {
		return err
	}
	if ledgers == 0 {
		if objects != 0 {
			return errors.New("unversioned databases are unsupported; use a new database or a PwnMesh schema version 2, 3 or 4 database")
		}
		if _, err := tx.Exec(migrationLedgerSchema); err != nil {
			return err
		}
		if err := createStoreSchema(tx); err != nil {
			return err
		}
		for n, name := range storeSchemaHistory {
			if _, err := tx.Exec("INSERT INTO pwnmesh_schema_migrations(version,name) VALUES(?,?)", n+1, name); err != nil {
				return err
			}
		}
	} else {
		version, err := validateStoreHistory(tx)
		if err != nil {
			return err
		}
		if err := validatePwnMeshNamespaceVersion(tx, version); err != nil {
			return err
		}
		if err := validateExecutionMetadata(tx); err != nil {
			return err
		}
		if version == 2 {
			if err := upgradeStateStorage(tx); err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT INTO pwnmesh_schema_migrations(version,name) VALUES(3,?)", storeSchemaHistory[2]); err != nil {
				return err
			}
		}
		if version < 4 {
			if err := upgradeStateRecords(tx); err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT INTO pwnmesh_schema_migrations(version,name) VALUES(4,?)", storeSchemaHistory[3]); err != nil {
				return err
			}
		}
	}
	if err := validatePwnMeshNamespace(tx); err != nil {
		return err
	}
	if err := validateExecutionMetadata(tx); err != nil {
		return err
	}
	if err := repairStoreRows(tx); err != nil {
		return err
	}
	if err := repairStoreCounters(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(pruneDecisionReadViews); err != nil {
		return err
	}
	return tx.Commit()
}

func validateExecutionMetadata(tx *sql.Tx) error {
	var metadataVersion int
	err := tx.QueryRow("SELECT metadata_version FROM pwnmesh_executions WHERE metadata_version!=1 LIMIT 1").Scan(&metadataVersion)
	if err == nil {
		return fmt.Errorf("unsupported execution metadata version %d; expected 1", metadataVersion)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

func validateStoreHistory(tx *sql.Tx) (int, error) {
	rows, err := tx.Query("SELECT version,name FROM pwnmesh_schema_migrations ORDER BY version")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	applied := 0
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			return 0, err
		}
		if version > len(storeSchemaHistory) {
			return 0, fmt.Errorf("unsupported PwnMesh schema version %d (current: %d)", version, len(storeSchemaHistory))
		}
		if version != applied+1 || name != storeSchemaHistory[applied] {
			return 0, fmt.Errorf("inconsistent PwnMesh migration history at version %d", version)
		}
		applied++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if applied == 0 {
		return 0, errors.New("inconsistent PwnMesh migration history: missing baseline")
	}
	if applied < 2 {
		return 0, fmt.Errorf("unsupported PwnMesh schema version %d (current: %d)", applied, len(storeSchemaHistory))
	}
	return applied, nil
}

func createStoreSchema(tx *sql.Tx) error {
	_, err := tx.Exec(schema + stateSchema + executionSchema + projectMetadataSchema + restartSchema + terminationSchema + orchestrationSchema + inputFileSchema + assetSchema + deploymentSchema + factMetadataSchema + stateRecordsSchema + executionWriteOwnerSchema + scheduleCacheSchema + scheduleInvalidationSchema())
	return err
}

func upgradeStateStorage(tx *sql.Tx) error {
	if _, err := tx.Exec(factMetadataSchema + scheduleCacheSchema); err != nil {
		return err
	}
	rows, err := tx.Query("SELECT project_id FROM pwnmesh_state ORDER BY project_id")
	if err != nil {
		return err
	}
	var projects []string
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			rows.Close()
			return err
		}
		projects = append(projects, project)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, project := range projects {
		var raw string
		if err := tx.QueryRow("SELECT data FROM pwnmesh_state WHERE project_id=?", project).Scan(&raw); err != nil {
			return err
		}
		remainder, facts, err := splitStateFacts(raw)
		if err != nil {
			return fmt.Errorf("migrate Fact metadata for %s: %w", project, err)
		}
		if remainder != raw {
			if _, err := tx.Exec("UPDATE pwnmesh_state SET data=? WHERE project_id=?", remainder, project); err != nil {
				return err
			}
			if err := saveStateFacts(tx, project, facts); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(scheduleInvalidationSchemaVersion(3, ""))
	return err
}

func upgradeStateRecords(tx *sql.Tx) error {
	if _, err := tx.Exec(stateRecordsSchema + executionWriteOwnerSchema); err != nil {
		return err
	}
	rows, err := tx.Query("SELECT project_id FROM pwnmesh_state ORDER BY project_id")
	if err != nil {
		return err
	}
	var projects []string
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			rows.Close()
			return err
		}
		projects = append(projects, project)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, project := range projects {
		var raw string
		if err := tx.QueryRow("SELECT data FROM pwnmesh_state WHERE project_id=?", project).Scan(&raw); err != nil {
			return err
		}
		header, records, err := splitStateRecords(raw)
		if err != nil {
			return fmt.Errorf("migrate state records for %s: %w", project, err)
		}
		if header != raw {
			if _, err := tx.Exec("UPDATE pwnmesh_state SET data=? WHERE project_id=?", header, project); err != nil {
				return err
			}
			if err := saveStateRecords(tx, project, records); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(scheduleInvalidationSchemaVersion(4, "pwnmesh_state_records"))
	return err
}

// Restore derived/default rows without rewriting records or recreating schema
// objects. Existing installation ownership stays unchanged.
func repairStoreRows(tx *sql.Tx) error {
	_, err := tx.Exec(settingsSeed + installationSeed + executionChangeBackfill)
	return err
}

func repairStoreCounters(tx *sql.Tx) error {
	// Restored current data can outgrow its counters. Never reuse a retained ID.
	if _, err := tx.Exec(`INSERT OR IGNORE INTO counters(name,value) VALUES('project',0);
UPDATE counters SET value=MAX(value,COALESCE((SELECT MAX(CAST(SUBSTR(id,6) AS INTEGER)) FROM projects WHERE id GLOB 'proj_[0-9]*'),0)) WHERE name='project'`); err != nil {
		return err
	}
	for _, item := range []struct{ table, kind, prefix string }{{"facts", "fact", "f"}, {"intents", "intent", "i"}, {"hints", "hint", "h"}} {
		query := fmt.Sprintf(`INSERT INTO scoped_counters(project_id,kind,value) SELECT project_id,?,MAX(CAST(SUBSTR(id,2) AS INTEGER)) FROM %s WHERE id GLOB ? GROUP BY project_id ON CONFLICT(project_id,kind) DO UPDATE SET value=MAX(value,excluded.value)`, item.table)
		if _, err := tx.Exec(query, item.kind, item.prefix+"[0-9]*"); err != nil {
			return err
		}
	}
	return nil
}
