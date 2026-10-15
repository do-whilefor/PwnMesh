package sqlstore

import (
	"database/sql"
	"errors"
	"fmt"
)

// The complete ledger identifies the current format. Historical entries are
// retained for integrity; they do not authorize opening an older schema.
var storeSchemaHistory = [...]string{"unversioned-baseline", "pwnmesh-namespace", "incremental-state-storage", "normalized-state-records"}

const migrationLedgerSchema = `CREATE TABLE pwnmesh_schema_migrations(
 version INTEGER PRIMARY KEY CHECK(version > 0),name TEXT NOT NULL,
 applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')));`

// Only empty stores initialize. Existing stores must have the complete current
// schema before maintenance runs; no old schema is adopted or upgraded.
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
			return errors.New("unversioned databases are unsupported; use a new database or a PwnMesh schema version 4 database")
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
		if err := validateStoreHistory(tx); err != nil {
			return err
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

func validateStoreHistory(tx *sql.Tx) error {
	rows, err := tx.Query("SELECT version,name FROM pwnmesh_schema_migrations ORDER BY version")
	if err != nil {
		return err
	}
	defer rows.Close()
	applied := 0
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			return err
		}
		if version > len(storeSchemaHistory) {
			return fmt.Errorf("unsupported PwnMesh schema version %d (current: %d)", version, len(storeSchemaHistory))
		}
		if version != applied+1 || name != storeSchemaHistory[applied] {
			return fmt.Errorf("inconsistent PwnMesh migration history at version %d", version)
		}
		applied++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if applied == 0 {
		return errors.New("inconsistent PwnMesh migration history: missing baseline")
	}
	if applied != len(storeSchemaHistory) {
		return fmt.Errorf("unsupported PwnMesh schema version %d (current: %d)", applied, len(storeSchemaHistory))
	}
	return nil
}

func createStoreSchema(tx *sql.Tx) error {
	_, err := tx.Exec(schema + stateSchema + executionSchema + projectMetadataSchema + restartSchema + terminationSchema + orchestrationSchema + inputFileSchema + assetSchema + deploymentSchema + factMetadataSchema + stateRecordsSchema + executionWriteOwnerSchema + scheduleCacheSchema + scheduleInvalidationSchema())
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
