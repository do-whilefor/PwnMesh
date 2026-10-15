package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestStepMetadataLookupPreservesScopeOrderAndUsesIndex(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Do(context.Background(), func(q Queries) error {
		if _, err := q.Exec("INSERT INTO projects(id,title,created_at) VALUES('p','p','now'),('other','other','now')"); err != nil {
			return err
		}
		if err := SaveStateMetadata(q, "p", `{"steps":[{"id":"a","priority":1},{"id":"unrelated","priority":"not decoded"},{"id":"b","priority":2},{"id":"a","priority":3},null,{"priority":4}],"goals":[{"id":"a"}]}`, 0, 0); err != nil {
			return err
		}
		if err := SaveStateMetadata(q, "other", `{"steps":[{"id":"a","priority":99}]}`, 0, 0); err != nil {
			return err
		}
		for _, test := range []struct {
			ids  []string
			want []string
		}{
			{[]string{"b", "a", "a"}, []string{`{"id":"a","priority":1}`, `{"id":"b","priority":2}`, `{"id":"a","priority":3}`}},
			{[]string{""}, []string{`null`, `{"priority":4}`}},
			{[]string{"absent"}, nil},
			{nil, nil},
		} {
			got, err := StateStepMetadataByID(q, "p", test.ids)
			var raw []string
			for _, row := range got {
				raw = append(raw, string(row))
			}
			if err != nil || !reflect.DeepEqual(raw, test.want) {
				t.Fatalf("selected %v: %v, want %v: %v", test.ids, raw, test.want, err)
			}
		}
		rows, err := q.Query("EXPLAIN QUERY PLAN "+stepMetadataByIDQuery, "p", `["a","b"]`)
		if err != nil {
			return err
		}
		indexed := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				return err
			}
			indexed = indexed || strings.Contains(detail, "pwnmesh_state_steps_id") && strings.Contains(detail, "record_id=?")
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if !indexed {
			t.Fatal("step selection did not seek by project and Step ID")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStepMetadataLookupMatchesGoIDDecoding(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const metadata = `{"steps":[{"ID":"upper"},{"Id":"mixed"},{"id":"first","id":"last"},{"id":"before","ID":"after"},{"ID":"retained","id":null},{"priority":1},null]}`
	if err := s.Do(context.Background(), func(q Queries) error {
		if _, err := q.Exec("INSERT INTO projects(id,title,created_at) VALUES('p','p','now')"); err != nil {
			return err
		}
		if err := SaveStateMetadata(q, "p", metadata, 0, 0); err != nil {
			return err
		}
		var original struct {
			Steps []json.RawMessage `json:"steps"`
		}
		if err := json.Unmarshal([]byte(metadata), &original); err != nil {
			return err
		}
		for _, id := range []string{"upper", "mixed", "first", "last", "before", "after", "retained", ""} {
			var want []json.RawMessage
			for _, raw := range original.Steps {
				var step struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(raw, &step); err != nil {
					return err
				}
				if step.ID == id {
					want = append(want, raw)
				}
			}
			got, err := StateStepMetadataByID(q, "p", []string{id})
			if len(got) == 0 && len(want) == 0 {
				got = nil
			}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("id=%q: got %s, want %s: %v", id, got, want, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type stepOwnerPlanQueries struct {
	Queries
	plan *[]string
}

func (q stepOwnerPlanQueries) Query(query string, args ...any) (*sql.Rows, error) {
	rows, err := q.Queries.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			rows.Close()
			return nil, err
		}
		*q.plan = append(*q.plan, detail)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return q.Queries.Query(query, args...)
}

func TestStepWriteOwnersSeekOnlyLiveProjectExecutions(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Do(context.Background(), func(q Queries) error {
		if _, err := q.Exec(`INSERT INTO projects(id,title,created_at) VALUES('p','p','now'),('other','other','now');
INSERT INTO intents(id,project_id,description,creator,created_at,worker) VALUES('leased','p','work','test','now','live');`); err != nil {
			return err
		}
		for _, run := range []model.ExecutionSummary{
			{ProjectID: "p", ID: "duplicate", Kind: "explore", Intent: "leased", Lease: "live", Status: "running"},
			{ProjectID: "p", ID: "orphan", Kind: "explore", Intent: "orphan", Lease: "pending", Status: "result_pending"},
			{ProjectID: "p", ID: "historical", Kind: "explore", Intent: "historical", Lease: "ended", Status: "succeeded"},
			{ProjectID: "p", ID: "reason", Kind: "reason", Intent: "reason", Lease: "planner", Status: "running"},
			{ProjectID: "other", ID: "foreign", Kind: "explore", Intent: "foreign", Lease: "other", Status: "running"},
		} {
			putExecutionSummary(t, q, run)
		}
		var plan, owners []string
		if err := VisitStepWriteOwners(stepOwnerPlanQueries{Queries: q, plan: &plan}, "p", func(id, worker string) error {
			owners = append(owners, id+":"+worker)
			return nil
		}); err != nil {
			return err
		}
		if !reflect.DeepEqual(owners, []string{"leased:live", "orphan:pending"}) {
			t.Fatalf("live owner scope or duplicate handling changed: %v", owners)
		}
		if !strings.Contains(strings.Join(plan, "\n"), "pwnmesh_execution_write_owners (project_id=?)") {
			t.Fatalf("write owners scanned historical or other-project executions: %v", plan)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
