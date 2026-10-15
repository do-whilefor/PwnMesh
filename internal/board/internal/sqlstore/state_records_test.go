package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board/internal/projection"
	"pwnmesh/internal/board/model"
)

func TestStateMetadataCollectionsWriteOnlyChangedRecords(t *testing.T) {
	store := actionReceiptStore(t)
	const initial = `{"goals":[{"id":"same","description":"first"},{"id":"same","description":"duplicate"}],"steps":[{"id":"orphan","priority":7}],"findings":[{"id":"finding"}],"fact_relations":[{"id":"relation"}],"candidates":[{"id":"candidate"}],"disputes":[{"id":"dispute"}],"facts":[{"id":"fact"}],"unknown":9007199254740993}`
	changed := strings.Replace(initial, `"priority":7`, `"priority":8`, 1)
	if err := store.Do(context.Background(), func(q Queries) error {
		if err := SaveStateMetadata(q, "p", initial, 4, 2); err != nil {
			return err
		}
		if err := SaveStateMetadata(q, "other", initial, 4, 2); err != nil {
			return err
		}
		var header string
		if err := q.QueryRow("SELECT data FROM pwnmesh_state WHERE project_id='p'").Scan(&header); err != nil {
			return err
		}
		if strings.Contains(header, "description") || strings.Contains(header, "priority") || strings.Contains(header, "candidate\"") {
			t.Fatalf("collection payload remains in the project JSON: %s", header)
		}
		if _, err := q.Exec(`CREATE TRIGGER guard_unchanged_record BEFORE INSERT ON pwnmesh_state_records WHEN NEW.kind!='steps' BEGIN SELECT RAISE(ABORT,'unchanged record inserted'); END;
CREATE TRIGGER guard_unchanged_record_update BEFORE UPDATE ON pwnmesh_state_records WHEN NEW.data=OLD.data BEGIN SELECT RAISE(ABORT,'unchanged record updated'); END;`); err != nil {
			return err
		}
		if err := SaveStateMetadata(q, "p", changed, 5, 3); err != nil {
			return err
		}
		if err := UpdateStateMetadata(q, "p", changed); err != nil {
			return err
		}
		raw, revision, decision, err := StateMetadata(q, "p")
		if err != nil {
			return err
		}
		var got, want map[string]json.RawMessage
		if json.Unmarshal(raw, &got) != nil || json.Unmarshal([]byte(changed), &want) != nil || !reflect.DeepEqual(got, want) || revision != 5 || decision != 3 {
			t.Fatalf("records changed ordering, duplicate IDs, unknown integers or revisions: %s %d/%d", raw, revision, decision)
		}
		if err := DeleteStateRecords(q, "p"); err != nil {
			return err
		}
		var current, other int
		if err := q.QueryRow("SELECT COUNT(*) FROM pwnmesh_state_records WHERE project_id='p'").Scan(&current); err != nil {
			return err
		}
		if err := q.QueryRow("SELECT COUNT(*) FROM pwnmesh_state_records WHERE project_id='other'").Scan(&other); err != nil {
			return err
		}
		if current != 0 || other != 7 {
			t.Fatalf("record cascade escaped project scope: %d/%d", current, other)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStateMetadataCollectionShapesAndTruncation(t *testing.T) {
	store := actionReceiptStore(t)
	if err := store.Do(context.Background(), func(q Queries) error {
		for _, kind := range stateRecordKinds {
			for _, value := range []string{`[{"id":"duplicate"},{"id":"duplicate"},{"id":"orphan"}]`, `[{"id":"kept"}]`, `[]`, `null`, ""} {
				raw := `{"unknown":9007199254740993}`
				if value != "" {
					raw = fmt.Sprintf(`{"unknown":9007199254740993,%q:%s}`, kind, value)
				}
				if err := SaveStateMetadata(q, "p", raw, 1, 1); err != nil {
					return err
				}
				restored, _, _, err := StateMetadata(q, "p")
				if err != nil {
					return err
				}
				var got, want map[string]json.RawMessage
				if json.Unmarshal(restored, &got) != nil || json.Unmarshal([]byte(raw), &want) != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("%s shape %q changed: %s", kind, value, restored)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStateMetadataRecordFailureRestoresFactsHeaderAndSchedule(t *testing.T) {
	for _, operation := range []string{"save", "update"} {
		t.Run(operation, func(t *testing.T) {
			store := actionReceiptStore(t)
			const original = `{"goals":[{"id":"original"}],"facts":[{"id":"original"}],"steps":[{"id":"first"},{"id":"second"}]}`
			const changed = `{"goals":[{"id":"changed"}],"facts":[{"id":"changed"}],"steps":[{"id":"changed"},{"id":"rejected"}]}`
			var before []byte
			if err := store.Do(context.Background(), func(q Queries) error {
				if err := SaveStateMetadata(q, "p", original, 4, 2); err != nil {
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
				if _, err := q.Exec(`CREATE TRIGGER reject_state_record BEFORE INSERT ON pwnmesh_state_records WHEN NEW.kind='steps' AND NEW.position=1 BEGIN SELECT RAISE(ABORT,'second Step rejected'); END`); err != nil {
					return err
				}
				if operation == "save" {
					err = SaveStateMetadata(q, "p", changed, 9, 7)
				} else {
					err = UpdateStateMetadata(q, "p", changed)
				}
				if err == nil || !strings.Contains(err.Error(), "second Step rejected") {
					t.Fatalf("record failure did not reach caller: %v", err)
				}
				return nil // An outer caller handles the failure and commits.
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.View(context.Background(), func(q Queries) error {
				raw, revision, decision, err := StateMetadata(q, "p")
				if err != nil || string(raw) != string(before) || revision != 4 || decision != 2 {
					t.Fatalf("handled failure changed committed metadata: %s %d/%d %v", raw, revision, decision, err)
				}
				header, count, err := ScheduleHeader(q, "p")
				if err != nil || string(header) != `{"original":true}` || count != 1 {
					t.Fatalf("handled failure invalidated the committed schedule: %s %d %v", header, count, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Build the actual v3 tables and triggers, without current migration helpers or
// any v4 objects. Retained v3 Facts already live outside pwnmesh_state.data.
func createVersionThreeFixture(t *testing.T, path, extra string) {
	t.Helper()
	createVersionTwoFixture(t, path, "")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var triggers strings.Builder
	for _, table := range []string{
		"projects", "facts", "intents", "intent_sources", "hints",
		"pwnmesh_state", "pwnmesh_fact_metadata", "pwnmesh_state_events",
		"pwnmesh_project_metadata", "pwnmesh_project_rounds", "pwnmesh_project_termination",
		"pwnmesh_project_orchestration", "pwnmesh_executions", "pwnmesh_revoked_runs",
		"pwnmesh_assets", "pwnmesh_asset_anchors",
	} {
		for _, op := range []string{"insert", "update", "delete"} {
			if table == "projects" && op != "update" {
				continue
			}
			column := "project_id"
			if table == "projects" {
				column = "id"
			}
			ref := "NEW." + column
			if op == "delete" {
				ref = "OLD." + column
			} else if op == "update" {
				ref += ",OLD." + column
			}
			fmt.Fprintf(&triggers, "CREATE TRIGGER pwnmesh_schedule_invalidate_%s_%s AFTER %s ON %s BEGIN DELETE FROM pwnmesh_schedule_cache WHERE project_id IN (%s); END;\n", table, op, op, table, ref)
		}
	}
	const versionThreeTables = `CREATE TABLE pwnmesh_fact_metadata(project_id TEXT NOT NULL REFERENCES pwnmesh_state(project_id) ON DELETE CASCADE,position INTEGER NOT NULL CHECK(position>=0),data TEXT NOT NULL,PRIMARY KEY(project_id,position));
CREATE TABLE pwnmesh_schedule_cache(project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,header BLOB NOT NULL,step_count INTEGER NOT NULL CHECK(step_count>=0));
CREATE TABLE pwnmesh_schedule_steps(project_id TEXT NOT NULL REFERENCES pwnmesh_schedule_cache(project_id) ON DELETE CASCADE,position INTEGER NOT NULL CHECK(position>=0),data BLOB NOT NULL,PRIMARY KEY(project_id,position));`
	if _, err := db.Exec(versionThreeTables + triggers.String() + `INSERT INTO pwnmesh_schema_migrations VALUES(3,'incremental-state-storage','original');
INSERT INTO pwnmesh_fact_metadata(rowid,project_id,position,data) VALUES(77,'project',0,' {"id":"fact","unknown":9007199254740993} ');` + extra); err != nil {
		t.Fatal(err)
	}
}

func TestVersionThreeUpgradePreservesOrderedRecordsAndRetainedBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version-three.db")
	const metadata = `{"goals":[{"id":"duplicate"},{"id":"duplicate"}],"steps":[{"id":"first","ID":"orphan","priority":7}],"findings":[{"id":"finding"}],"fact_relations":[{"id":"relation"}],"candidates":[{"id":"candidate"}],"disputes":[{"id":"dispute"}],"unknown":9007199254740993}`
	createVersionThreeFixture(t, path, "UPDATE pwnmesh_state SET data='"+metadata+"';")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.View(context.Background(), func(q Queries) error {
		raw, revision, decision, err := StateMetadata(q, "project")
		if err != nil {
			return err
		}
		var got, want map[string]json.RawMessage
		if json.Unmarshal(raw, &got) != nil || json.Unmarshal([]byte(metadata), &want) != nil {
			t.Fatal("invalid restored metadata")
		}
		want["facts"] = json.RawMessage(`[{"id":"fact","unknown":9007199254740993}]`)
		if !reflect.DeepEqual(got, want) || revision != 9 || decision != 7 {
			t.Fatalf("migration changed metadata or revisions: %s %d/%d", raw, revision, decision)
		}
		var count, rowid int
		var fact string
		if err := q.QueryRow("SELECT rowid,data FROM pwnmesh_fact_metadata WHERE project_id='project'").Scan(&rowid, &fact); err != nil || rowid != 77 || fact != ` {"id":"fact","unknown":9007199254740993} ` {
			t.Fatalf("v4 rewrote v3 Fact bytes: %d %q %v", rowid, fact, err)
		}
		if err := q.QueryRow("SELECT COUNT(*) FROM pwnmesh_state_records").Scan(&count); err != nil || count != 7 {
			t.Fatalf("v4 did not normalize every collection: %d %v", count, err)
		}
		var stepID, stepData string
		if err := q.QueryRow("SELECT record_id,data FROM pwnmesh_state_records WHERE project_id='project' AND kind='steps'").Scan(&stepID, &stepData); err != nil || stepID != "orphan" || stepData != `{"id":"first","ID":"orphan","priority":7}` {
			t.Fatalf("v4 changed Go Step identity or raw metadata: %q %q %v", stepID, stepData, err)
		}
		if err := q.QueryRow("SELECT COUNT(*) FROM pwnmesh_schema_migrations WHERE version<=3 AND applied_at='original'").Scan(&count); err != nil || count != 3 {
			t.Fatalf("v4 rewrote v3 migration history: %d %v", count, err)
		}
		var job, result, snapshot, pinned []byte
		if err := q.QueryRow("SELECT job,result FROM pwnmesh_executions WHERE rowid=70").Scan(&job, &result); err != nil {
			return err
		}
		if err := q.QueryRow("SELECT state FROM pwnmesh_input_snapshots WHERE id='snapshot'").Scan(&snapshot); err != nil {
			return err
		}
		if err := q.QueryRow("SELECT state FROM pwnmesh_decision_read_views WHERE execution_id='later'").Scan(&pinned); err != nil {
			return err
		}
		if string(job) != ` {"job":true} ` || !reflect.DeepEqual(result, []byte{0, 'x', 255}) || !reflect.DeepEqual(snapshot, result) || !reflect.DeepEqual(pinned, result) {
			t.Fatalf("v4 rewrote retained execution or input bytes: %q %v %v %v", job, result, snapshot, pinned)
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
	if after := currentStoreRows(t, store.db); !reflect.DeepEqual(before, after) {
		t.Fatal("reopen changed v4 schema or retained bytes")
	}
}

func TestVersionThreeUpgradeRejectsDamageAndRollsBack(t *testing.T) {
	for _, test := range []struct{ name, extra, message string }{
		{"invalid_collection", `UPDATE pwnmesh_state SET data='{"steps":{"invalid":true}}';`, "migrate state records"},
		{"invalid_identity", `UPDATE pwnmesh_state SET data='{"steps":[{"id":42}]}';`, "invalid Step identity"},
		{"unexpected_table", `CREATE TABLE pwnmesh_state_records(id INTEGER);`, "namespace"},
		{"backfill_failure", `INSERT INTO projects(id,title,created_at) VALUES('aaa','first','now'); INSERT INTO pwnmesh_state VALUES('aaa','{"goals":[{"id":"first"}]}',3,2);
UPDATE pwnmesh_state SET data='{"steps":[{"id":"second"}]}' WHERE project_id='project';
CREATE TRIGGER reject_record_migration BEFORE UPDATE OF data ON pwnmesh_state WHEN NEW.project_id='project' BEGIN SELECT RAISE(ABORT,'record migration rejected'); END;`, "record migration rejected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rejected.db")
			createVersionThreeFixture(t, path, test.extra)
			assertStoreRejectedUnchanged(t, path, test.message)
		})
	}
}

func assertInlineMetadata(t *testing.T, q Queries, project, original string) {
	t.Helper()
	raw, _, _, err := StateMetadata(q, project)
	if err != nil || string(raw) != original {
		t.Fatalf("historical header bytes changed: %s, want %s: %v", raw, original, err)
	}
	want, err := projection.DecodeMetadata([]byte(original))
	if err != nil {
		t.Fatal(err)
	}
	got, err := projection.DecodeMetadata(raw)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("historical field precedence or struct merging changed: %+v, want %+v: %v", got, want, err)
	}
	var count int
	if err := q.QueryRow("SELECT (SELECT COUNT(*) FROM pwnmesh_state_records WHERE project_id=?)+(SELECT COUNT(*) FROM pwnmesh_fact_metadata WHERE project_id=?)", project, project).Scan(&count); err != nil || count != 0 {
		t.Fatalf("historical aliases were partly normalized: %d %v", count, err)
	}
}

func TestHistoricalStateHeadersRetainAliasesAndRepeatedFields(t *testing.T) {
	store := actionReceiptStore(t)
	var headers []string
	for _, kind := range append(stateRecordKinds[:], "facts", "curation") {
		base := `{"unknown":9007199254740993`
		if kind != "facts" {
			base += `,"facts":[{"id":"fact"}]`
		}
		if kind != "steps" {
			base += `,"steps":[{"id":"step"}]`
		}
		first, last := `[{"id":"first"}]`, `[{"id":"last"}]`
		if kind == "curation" {
			first, last = `{"generation":9}`, `{"through_revision":4}`
		}
		headers = append(headers, fmt.Sprintf(`%s,%q:%s,%q:%s}`, base, kind, first, strings.ToUpper(kind), last))
	}
	headers = append(headers, `{"facts":[{"id":"fact"}],"steps":[{"id":"step"}],"curation":{"generation":9},"curation":{"through_revision":4}}`)
	if err := store.Do(context.Background(), func(q Queries) error {
		for _, raw := range headers {
			for _, update := range []bool{false, true} {
				if err := SaveStateMetadata(q, "p", `{"goals":[{"id":"before"}],"steps":[{"id":"before"}],"facts":[{"id":"before"}]}`, 1, 1); err != nil {
					return err
				}
				var err error
				if update {
					err = UpdateStateMetadata(q, "p", raw)
				} else {
					err = SaveStateMetadata(q, "p", raw, 2, 2)
				}
				if err != nil {
					return err
				}
				assertInlineMetadata(t, q, "p", raw)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalStateHeadersSurviveVersionTwoAndThreeUpgrade(t *testing.T) {
	for _, version := range []int{2, 3} {
		for _, raw := range []string{
			` { "facts":[{"id":"fact"}],"steps":[{"id":"first"}],"Steps":[{"id":"last"}],"curation":{"generation":9},"Curation":{"through_revision":4} } `,
			` { "facts":[{"id":"fact"}],"steps":[{"id":"step"}],"curation":{"generation":9},"curation":{"through_revision":4} } `,
		} {
			t.Run(fmt.Sprintf("v%d/%s", version, raw[3:10]), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "historical.db")
				extra := "UPDATE pwnmesh_state SET data='" + raw + "';"
				if version == 2 {
					createVersionTwoFixture(t, path, extra)
				} else {
					createVersionThreeFixture(t, path, "DELETE FROM pwnmesh_fact_metadata;"+extra)
				}
				store, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				if err := store.View(context.Background(), func(q Queries) error {
					assertInlineMetadata(t, q, "project", raw)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
