package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board/model"
)

type metadataTrimQueries struct {
	Queries
	deletes int
}

func (q *metadataTrimQueries) Exec(query string, args ...any) (sql.Result, error) {
	if strings.HasPrefix(query, "DELETE FROM pwnmesh_state_records ") || strings.HasPrefix(query, "DELETE FROM pwnmesh_fact_metadata ") {
		q.deletes++
	}
	return q.Queries.Exec(query, args...)
}

func TestStateMetadataTrimsOnlyRetainedTailRecords(t *testing.T) {
	store := actionReceiptStore(t)
	const initial = `{"steps":[{"id":"first"}],"facts":[{"id":"first"}]}`
	const appended = `{"steps":[{"id":"first"},{"id":"second"}],"facts":[{"id":"first"},{"id":"second"}]}`
	if err := store.Do(context.Background(), func(q Queries) error {
		if err := SaveStateMetadata(q, "other", initial, 1, 1); err != nil {
			return err
		}
		tracked := &metadataTrimQueries{Queries: q}
		save := func(raw string, deletes int) error {
			tracked.deletes = 0
			if err := SaveStateMetadata(tracked, "p", raw, 1, 1); err != nil {
				return err
			}
			if tracked.deletes != deletes {
				t.Fatalf("metadata issued %d tail deletes, want %d for %s", tracked.deletes, deletes, raw)
			}
			restored, _, _, err := StateMetadata(q, "p")
			if err != nil {
				return err
			}
			var got, want any
			if json.Unmarshal(restored, &got) != nil || json.Unmarshal([]byte(raw), &want) != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("tail trimming changed metadata: %s, want %s", restored, raw)
			}
			return nil
		}
		// Preparing a no-op DELETE still compiles its SQLite trigger programs.
		// New, repeated and appended metadata must avoid all such statements.
		for _, raw := range []string{initial, initial, appended} {
			if err := save(raw, 0); err != nil {
				return err
			}
		}
		// Historical positions can be sparse: row counts cannot identify tails.
		if _, err := q.Exec(`UPDATE pwnmesh_state_records SET position=5 WHERE project_id='p' AND kind='steps' AND position=1;
UPDATE pwnmesh_fact_metadata SET position=5 WHERE project_id='p' AND position=1;`); err != nil {
			return err
		}
		if err := save(appended, 2); err != nil {
			return err
		}
		if err := save(`{}`, 2); err != nil {
			return err
		}
		if err := save(`{}`, 0); err != nil {
			return err
		}
		raw, _, _, err := StateMetadata(q, "other")
		if err != nil {
			return err
		}
		var got, want any
		if json.Unmarshal(raw, &got) != nil || json.Unmarshal([]byte(initial), &want) != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("tail trimming changed another project's metadata: %s", raw)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

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

func TestStateMetadataRejectsUnsupportedHeadersWithoutMutation(t *testing.T) {
	store := actionReceiptStore(t)
	headers := []string{``, `null`, `[]`, `{"steps":[]`, `{} {}`}
	for _, kind := range append(stateRecordKinds[:], "facts", "curation") {
		headers = append(headers, fmt.Sprintf(`{%q:null}`, strings.ToUpper(kind)), fmt.Sprintf(`{%q:null,%q:null}`, kind, kind))
	}
	if err := store.Do(context.Background(), func(q Queries) error {
		if err := SaveStateMetadata(q, "p", `{"steps":[{"id":"retained"}],"facts":[{"id":"retained"}]}`, 1, 1); err != nil {
			return err
		}
		before, _, _, err := StateMetadata(q, "p")
		if err != nil {
			return err
		}
		var originalHeader string
		if err := q.QueryRow("SELECT data FROM pwnmesh_state WHERE project_id='p'").Scan(&originalHeader); err != nil {
			return err
		}
		for _, raw := range headers {
			for _, update := range []bool{false, true} {
				var err error
				if update {
					err = UpdateStateMetadata(q, "p", raw)
				} else {
					err = SaveStateMetadata(q, "p", raw, 2, 2)
				}
				if err == nil {
					t.Fatalf("unsupported header accepted: %q", raw)
				}
				after, revision, decision, err := StateMetadata(q, "p")
				if err != nil || string(before) != string(after) || revision != 1 || decision != 1 {
					t.Fatalf("rejected header changed records: %s, %d/%d, %v", after, revision, decision, err)
				}
			}
		}
		// Damaged current rows must not be treated as an empty Step collection.
		headers = append(headers, `{}`, `{"steps":null}`, `{"steps":[{"id":"inline"}]}`, `{"facts":[{"id":"inline"}]}`, `{"steps":[],"facts":null}`)
		for _, header := range headers {
			if _, err := q.Exec("UPDATE pwnmesh_state SET data=? WHERE project_id='p'", header); err != nil {
				return err
			}
			if _, _, _, err := StateMetadata(q, "p"); err == nil {
				t.Fatalf("noncurrent stored header accepted: %q", header)
			}
			if header != `{"steps":[],"facts":null}` {
				if _, err := StateStepMetadataByID(q, "p", []string{"retained"}); err == nil {
					t.Fatalf("Step lookup silently ignored a noncurrent header: %q", header)
				}
			}
		}
		_, err = q.Exec("UPDATE pwnmesh_state SET data=? WHERE project_id='p'", originalHeader)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentStateHeaderMarkersRemainReadable(t *testing.T) {
	store := actionReceiptStore(t)
	if err := store.Do(context.Background(), func(q Queries) error {
		for _, header := range []string{`{}`, `{"steps":null,"facts":null}`, `{"goals":[],"steps":[ ],"facts":[],"findings":[],"fact_relations":[]}`} {
			if err := SaveStateMetadata(q, "p", header, 1, 1); err != nil {
				return err
			}
			// Event initialization retains its current empty Fact marker.
			if _, err := q.Exec("UPDATE pwnmesh_state SET data=? WHERE project_id='p'", header); err != nil {
				return err
			}
			raw, _, _, err := StateMetadata(q, "p")
			if err != nil || string(raw) != header {
				t.Fatalf("current header marker changed: %s, %v", raw, err)
			}
			steps, err := StateStepMetadataByID(q, "p", []string{"absent"})
			if err != nil || len(steps) != 0 {
				t.Fatalf("empty current header returned Steps: %s, %v", steps, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
