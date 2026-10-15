package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestScheduleProjectionInvalidationAndBoundedRows(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Do(context.Background(), func(q Queries) error {
		if _, err := q.Exec(`INSERT INTO projects(id,title,created_at) VALUES('p','p','now');
INSERT INTO facts VALUES('f','p','fact');
INSERT INTO intents(id,project_id,description,creator,created_at) VALUES('i','p','work','test','now');
INSERT INTO intent_sources VALUES('i','p','f');
INSERT INTO hints VALUES('h','p','hint','test','now');
INSERT INTO pwnmesh_state VALUES('p','{}',0,0);
INSERT INTO pwnmesh_fact_metadata VALUES('p',0,'{}');
INSERT INTO pwnmesh_assets VALUES('p','a','host','example.test','');
INSERT INTO pwnmesh_asset_anchors VALUES('p',0,'fact','f','a');`); err != nil {
			return err
		}
		putExecutionSummary(t, q, model.ExecutionSummary{ProjectID: "p", ID: "run", Kind: "explore", Intent: "i", Status: "running"})
		for _, mutation := range []string{
			"UPDATE facts SET description='changed' WHERE project_id='p'",
			"DELETE FROM intent_sources WHERE project_id='p'",
			"UPDATE hints SET content='changed' WHERE project_id='p'",
			"UPDATE pwnmesh_fact_metadata SET data='{\"status\":\"invalid\"}' WHERE project_id='p'",
			"INSERT INTO pwnmesh_state_records(project_id,kind,position,data) VALUES('p','steps',0,'{\"id\":\"i\"}')",
			"UPDATE pwnmesh_state_records SET data='{\"id\":\"changed\"}' WHERE project_id='p'",
			"DELETE FROM pwnmesh_state_records WHERE project_id='p'",
			"INSERT INTO pwnmesh_state_events VALUES('p',1,'{}')",
			"UPDATE pwnmesh_executions SET status='failed' WHERE project_id='p'",
			"UPDATE pwnmesh_assets SET value='other.test' WHERE project_id='p'",
			"DELETE FROM pwnmesh_asset_anchors WHERE project_id='p'",
			"INSERT INTO pwnmesh_project_rounds VALUES('p',1,'now')",
			"INSERT INTO pwnmesh_project_termination VALUES('p','now')",
			"INSERT INTO pwnmesh_project_orchestration VALUES('p',1,'curator','input','now','now')",
			"INSERT INTO pwnmesh_project_metadata VALUES('p','ctf')",
			"INSERT INTO pwnmesh_revoked_runs VALUES('p','worker')",
		} {
			if err := SaveSchedule(q, "p", []byte(`{}`), []model.ScheduleStep{{ID: "i"}}); err != nil {
				return err
			}
			if _, err := q.Exec(mutation); err != nil {
				return err
			}
			if _, _, err := ScheduleHeader(q, "p"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("mutation retained stale projection: %s: %v", mutation, err)
			}
			var count int
			if err := q.QueryRow("SELECT COUNT(*) FROM pwnmesh_schedule_steps").Scan(&count); err != nil || count != 0 {
				t.Fatalf("invalidated projection retained rows: %d, %v", count, err)
			}
		}
		if err := SaveSchedule(q, "p", []byte(`{}`), []model.ScheduleStep{{ID: "first"}, {ID: "second"}, {ID: "third"}}); err != nil {
			return err
		}
		// Malformed rows outside the selected page must never reach the decoder.
		if _, err := q.Exec("UPDATE pwnmesh_schedule_steps SET data='invalid' WHERE position!=1"); err != nil {
			return err
		}
		page, err := ScheduleSteps(q, "p", 1, 1)
		if err != nil || len(page) != 1 || page[0].ID != "second" {
			t.Fatalf("page read unrelated rows: %+v, %v", page, err)
		}
		rows, err := q.Query("EXPLAIN QUERY PLAN SELECT data FROM pwnmesh_schedule_steps WHERE project_id=? AND position>=? ORDER BY position LIMIT ?", "p", 1, 1)
		if err != nil {
			return err
		}
		defer rows.Close()
		indexed := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				return err
			}
			indexed = indexed || strings.Contains(detail, "SEARCH pwnmesh_schedule_steps USING INDEX")
			if strings.Contains(detail, "TEMP B-TREE") {
				t.Fatalf("paging sorted the full projection: %s", detail)
			}
		}
		if !indexed {
			t.Fatal("paging did not seek through the project/position index")
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
}

func TestScheduleProjectionSaveFailureRollsBack(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Do(context.Background(), func(q Queries) error {
		if _, err := q.Exec("INSERT INTO projects(id,title,created_at) VALUES('p','p','now')"); err != nil {
			return err
		}
		if err := SaveSchedule(q, "p", []byte(`{"version":"original"}`), []model.ScheduleStep{{ID: "original"}}); err != nil {
			return err
		}
		if _, err := q.Exec(`CREATE TRIGGER reject_schedule BEFORE INSERT ON pwnmesh_schedule_steps WHEN NEW.position=1 BEGIN SELECT RAISE(ABORT,'rejected'); END`); err != nil {
			return err
		}
		if err := SaveSchedule(q, "p", []byte(`{}`), []model.ScheduleStep{{ID: "new"}, {ID: "rejected"}}); err == nil {
			t.Fatal("injected cache failure was ignored")
		}
		header, count, err := ScheduleHeader(q, "p")
		if err != nil || string(header) != `{"version":"original"}` || count != 1 {
			t.Fatalf("failed cache write changed the previous header: %s, %d, %v", header, count, err)
		}
		steps, err := ScheduleSteps(q, "p", 0, 100)
		if err != nil || len(steps) != 1 || steps[0].ID != "original" {
			t.Fatalf("failed cache write changed the previous steps: %+v, %v", steps, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
