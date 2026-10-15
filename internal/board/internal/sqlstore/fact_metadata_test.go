package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestFactMetadataWritesOnlyChangedRecordsAndCascades(t *testing.T) {
	store := actionReceiptStore(t)
	ctx := context.Background()
	const initial = `{"goals":[],"facts":[{"id":"f2","description":"first"},{"id":"f2","description":"duplicate"},{"id":"orphan","description":"retained"}]}`
	const appended = `{"goals":[{"id":"g1"}],"facts":[{"id":"f2","description":"first"},{"id":"f2","description":"duplicate"},{"id":"orphan","description":"retained"},{"id":"f3","description":"new"}]}`
	if err := store.Do(ctx, func(q Queries) error {
		if err := SaveStateMetadata(q, "p", initial, 4, 2); err != nil {
			return err
		}
		if err := SaveStateMetadata(q, "other", initial, 9, 7); err != nil {
			return err
		}
		for _, trigger := range []string{
			`CREATE TRIGGER guard_fact_insert BEFORE INSERT ON pwnmesh_fact_metadata WHEN NEW.position<3 BEGIN SELECT RAISE(ABORT,'unchanged Fact inserted'); END`,
			`CREATE TRIGGER guard_fact_update BEFORE UPDATE ON pwnmesh_fact_metadata BEGIN SELECT RAISE(ABORT,'unchanged Fact updated'); END`,
		} {
			if _, err := q.Exec(trigger); err != nil {
				return err
			}
		}
		if err := SaveStateMetadata(q, "p", appended, 5, 3); err != nil {
			return err
		}
		if err := UpdateStateMetadata(q, "p", appended); err != nil {
			return err
		}
		if _, err := AdvanceStateRevision(q, "p", "unused", 0); err != nil {
			return err
		}
		raw, revision, decision, err := StateMetadata(q, "p")
		if err != nil {
			return err
		}
		var got, want any
		if json.Unmarshal(raw, &got) != nil || json.Unmarshal([]byte(appended), &want) != nil || !reflect.DeepEqual(got, want) || revision != 6 || decision != 3 {
			t.Fatalf("split metadata lost ordering, duplicates or revisions: %s, %d/%d", raw, revision, decision)
		}
		var remainder string
		if err := q.QueryRow("SELECT data FROM pwnmesh_state WHERE project_id='p'").Scan(&remainder); err != nil {
			return err
		}
		if strings.Contains(remainder, `"facts"`) || strings.Contains(remainder, "description") {
			t.Fatalf("observations remained in project JSON: %s", remainder)
		}
		return DeleteStateRecords(q, "p")
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.View(ctx, func(q Queries) error {
		var current, other int
		if err := q.QueryRow("SELECT COUNT(*) FROM pwnmesh_fact_metadata WHERE project_id='p'").Scan(&current); err != nil {
			return err
		}
		if err := q.QueryRow("SELECT COUNT(*) FROM pwnmesh_fact_metadata WHERE project_id='other'").Scan(&other); err != nil {
			return err
		}
		if current != 0 || other != 3 {
			t.Fatalf("metadata cascade crossed project scope: %d/%d", current, other)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFactMetadataFailureRemainsAtomicWhenCallerCommits(t *testing.T) {
	for _, operation := range []string{"save", "update"} {
		t.Run(operation, func(t *testing.T) {
			store := actionReceiptStore(t)
			ctx := context.Background()
			const initial = `{"goals":[{"id":"original"}],"facts":[{"id":"f1","description":"original"},{"id":"f2","description":"original"}]}`
			const changed = `{"goals":[{"id":"changed"}],"facts":[{"id":"f1","description":"changed"},{"id":"f2","description":"rejected"}]}`
			var before []byte
			if err := store.Do(ctx, func(q Queries) error {
				if err := SaveStateMetadata(q, "p", initial, 4, 2); err != nil {
					return err
				}
				var err error
				before, _, _, err = StateMetadata(q, "p")
				if err != nil {
					return err
				}
				if err := SaveSchedule(q, "p", []byte(`{"original":true}`), []model.ScheduleStep{{ID: "original"}}); err != nil {
					return err
				}
				if _, err := q.Exec(`CREATE TRIGGER reject_second_fact BEFORE INSERT ON pwnmesh_fact_metadata WHEN NEW.position=1 BEGIN SELECT RAISE(ABORT,'second Fact rejected'); END`); err != nil {
					return err
				}
				if operation == "save" {
					err = SaveStateMetadata(q, "p", changed, 9, 7)
				} else {
					err = UpdateStateMetadata(q, "p", changed)
				}
				if err == nil || !strings.Contains(err.Error(), "second Fact rejected") {
					t.Fatalf("injected failure did not reach the caller: %v", err)
				}
				return nil // The caller handles the error and commits its transaction.
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.View(ctx, func(q Queries) error {
				after, revision, decision, err := StateMetadata(q, "p")
				if err != nil {
					return err
				}
				if string(after) != string(before) || revision != 4 || decision != 2 {
					t.Fatalf("handled failure committed partial metadata: %s %d/%d", after, revision, decision)
				}
				header, count, err := ScheduleHeader(q, "p")
				if err != nil || string(header) != `{"original":true}` || count != 1 {
					t.Fatalf("handled failure invalidated the committed schedule: %s %d %v", header, count, err)
				}
				steps, err := ScheduleSteps(q, "p", 0, 100)
				if err != nil || len(steps) != 1 || steps[0].ID != "original" {
					t.Fatalf("handled failure removed schedule rows: %+v %v", steps, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func createVersionTwoFixture(t *testing.T, path, extra string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	baseline := schema + stateSchema + executionSchema + projectMetadataSchema + restartSchema + terminationSchema + orchestrationSchema + inputFileSchema + assetSchema + deploymentSchema
	if _, err := db.Exec(migrationLedgerSchema + baseline + `INSERT INTO pwnmesh_schema_migrations VALUES(1,'unversioned-baseline','original'),(2,'pwnmesh-namespace','original');` + currentStoreData + extra); err != nil {
		t.Fatal(err)
	}
}

func TestVersionTwoUpgradePreservesMetadataAndHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version-two.db")
	createVersionTwoFixture(t, path, `UPDATE pwnmesh_state SET data=' { "unknown":9007199254740993,"steps":[{"id":"step","status":"abandoned"}],"facts":[{"id":"f2","description":"first"},{"id":"f2","description":"duplicate"},{"id":"orphan","description":"retained"}] } ';`)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.View(context.Background(), func(q Queries) error {
		var count int
		if err := q.QueryRow("SELECT COUNT(*) FROM pwnmesh_schema_migrations WHERE version<=2 AND applied_at='original'").Scan(&count); err != nil {
			return err
		}
		if count != 2 {
			t.Fatalf("upgrade rewrote baseline history: %d", count)
		}
		if err := q.QueryRow("SELECT COUNT(*) FROM pwnmesh_schema_migrations WHERE version=3 AND name='incremental-state-storage'").Scan(&count); err != nil || count != 1 {
			t.Fatalf("upgrade missing ledger: %d %v", count, err)
		}
		raw, revision, decision, err := StateMetadata(q, "project")
		if err != nil {
			return err
		}
		var data struct {
			Facts []struct{ ID, Description string }
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			return err
		}
		if len(data.Facts) != 3 || data.Facts[0].Description != "first" || data.Facts[1].Description != "duplicate" || data.Facts[2].ID != "orphan" || !strings.Contains(string(raw), "9007199254740993") || revision != 9 || decision != 7 {
			t.Fatalf("upgrade changed observations, unknown integer or revisions: %s, %d/%d", raw, revision, decision)
		}
		var job, result []byte
		if err := q.QueryRow("SELECT job,result FROM pwnmesh_executions WHERE rowid=70").Scan(&job, &result); err != nil {
			return err
		}
		if string(job) != ` {"job":true} ` || !reflect.DeepEqual(result, []byte{0, 'x', 255}) {
			t.Fatalf("upgrade changed execution rowid or bytes: %q %v", job, result)
		}
		return nil
	}); err != nil {
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
		t.Fatal("current reopen reapplied the upgrade or rewrote retained rows")
	}
}

func TestVersionTwoUpgradeRejectsDamageAndRollsBack(t *testing.T) {
	for _, test := range []struct{ name, extra, message string }{
		{"missing_table", "DROP TABLE pwnmesh_state;", "namespace"},
		{"unexpected_target", "CREATE TABLE pwnmesh_fact_metadata(id INTEGER);", "namespace"},
		{"invalid_facts", `UPDATE pwnmesh_state SET data='{"facts":{"invalid":true}}';`, "migrate Fact metadata"},
		{"backfill_failure", `UPDATE pwnmesh_state SET data='{"facts":[{"id":"f1"}]}'; CREATE TRIGGER reject_fact_migration BEFORE UPDATE OF data ON pwnmesh_state BEGIN SELECT RAISE(ABORT,'migration rejected'); END;`, "migration rejected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rejected.db")
			createVersionTwoFixture(t, path, test.extra)
			assertStoreRejectedUnchanged(t, path, test.message)
		})
	}
}
