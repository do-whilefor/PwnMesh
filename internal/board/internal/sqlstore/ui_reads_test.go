package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestUIReadsRetainCallerSnapshotAndBoundResultFields(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	result, err := json.Marshal(map[string]string{
		"text": strings.Repeat("\U0001f680", 65538), "error": strings.Repeat("\U0001f680", 4098),
		"thinking": strings.Repeat("private", 1<<16),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Do(ctx, func(q Queries) error {
		for _, id := range []string{"first", "second"} {
			putExecutionSummary(t, q, model.ExecutionSummary{ProjectID: "p", ID: id, Kind: "explore", Intent: "step", Status: "running", Generation: 3})
		}
		if _, err := q.Exec("INSERT INTO pwnmesh_project_rounds VALUES('p',3,'now')"); err != nil {
			return err
		}
		_, err := q.Exec("UPDATE pwnmesh_executions SET result=? WHERE id='first'", string(result))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := s.View(ctx, func(q Queries) error {
		id, generation, err := ProjectIdentity(q, "p")
		if err != nil || id != "p" || generation != 3 {
			t.Fatalf("project identity: %q %d, %v", id, generation, err)
		}
		through, err := LatestExecutionRow(q, "p")
		if err != nil {
			return err
		}
		revision, err = LatestExecutionRevision(q, "p")
		if err != nil {
			return err
		}
		if err := s.Do(ctx, func(writer Queries) error {
			if _, err := writer.Exec("UPDATE pwnmesh_executions SET status='failed',result='{}' WHERE id='first'"); err != nil {
				return err
			}
			if _, err := writer.Exec("DELETE FROM pwnmesh_executions WHERE id='second'"); err != nil {
				return err
			}
			if _, err := writer.Exec("UPDATE pwnmesh_project_rounds SET generation=4 WHERE project_id='p'"); err != nil {
				return err
			}
			putExecutionSummary(t, writer, model.ExecutionSummary{ProjectID: "p", ID: "late", Kind: "explore", Status: "running", Generation: 4})
			return nil
		}); err != nil {
			return err
		}
		var views []ExecutionViewRow
		if err := VisitExecutionViews(q, "p", 0, through, 10, func(row ExecutionViewRow) (bool, error) {
			views = append(views, row)
			return true, nil
		}); err != nil {
			return err
		}
		if len(views) != 2 || views[0].ID != "first" || views[1].ID != "second" || views[0].Status != "running" || views[0].Text != strings.Repeat("\U0001f680", 65537) || views[0].Failure != strings.Repeat("\U0001f680", 4097) {
			t.Fatal("UI view escaped its snapshot or lost the bounded UTF-8 sentinel")
		}
		changes := map[string]ExecutionViewRow{}
		if err := VisitExecutionChanges(q, "p", 0, revision, true, 10, func(row ExecutionChangeRow) (bool, error) {
			if row.Deleted {
				t.Fatal("change projection observed a newer deletion")
			}
			changes[row.ID] = row.View
			return true, nil
		}); err != nil {
			return err
		}
		if len(changes) != 2 || changes["first"] != views[0] || changes["second"] != views[1] {
			t.Fatal("change identities and projections used different snapshots")
		}
		summaries, err := ProjectSummaries(q)
		if err != nil || len(summaries) != 1 || summaries[0].Generation != 3 {
			t.Fatalf("summaries escaped snapshot: %+v, %v", summaries, err)
		}
		var traceIDs []string
		if err := VisitTraceRuns(q, "p", 3, 0, 10, "", func(run model.TraceRun) error {
			traceIDs = append(traceIDs, run.RunID)
			if run.Status != "running" || string(run.RegisteredJob) != "unavailable" {
				t.Fatal("trace storage changed the registered job or snapshot")
			}
			return nil
		}); err != nil {
			return err
		}
		if !reflect.DeepEqual(traceIDs, []string{"first", "second"}) {
			t.Fatalf("trace rows escaped snapshot: %v", traceIDs)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(q Queries) error {
		latest, err := LatestExecutionRevision(q, "p")
		if err != nil {
			return err
		}
		var ids, deleted []string
		if err := VisitExecutionChanges(q, "p", revision, latest, true, 10, func(row ExecutionChangeRow) (bool, error) {
			ids = append(ids, row.ID)
			if row.Deleted {
				deleted = append(deleted, row.ID)
			}
			return true, nil
		}); err != nil {
			return err
		}
		if !reflect.DeepEqual(ids, []string{"first", "second", "late"}) || !reflect.DeepEqual(deleted, []string{"second"}) {
			t.Fatalf("next snapshot missed changes or tombstones: %v, %v", ids, deleted)
		}
		_, _, err = ProjectIdentity(q, "absent")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("storage translated missing project: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUIVisitorsStopBeforeLaterRows(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Do(context.Background(), func(q Queries) error {
		for _, id := range []string{"first", "unreadable"} {
			putExecutionSummary(t, q, model.ExecutionSummary{ProjectID: "p", ID: id, Kind: "explore", Status: "running"})
		}
		// A later corrupt metadata row must not break a byte-limited page that
		// has already stopped. SQLite permits text in this INTEGER column.
		if _, err := q.Exec("UPDATE pwnmesh_executions SET resumes='invalid' WHERE id='unreadable'"); err != nil {
			return err
		}
		row, err := LatestExecutionRow(q, "p")
		if err != nil {
			return err
		}
		revision, err := LatestExecutionRevision(q, "p")
		if err != nil {
			return err
		}
		stop := errors.New("stop")
		for _, wantErr := range []error{nil, stop} {
			viewCount, changeCount := 0, 0
			err := VisitExecutionViews(q, "p", 0, row, 10, func(ExecutionViewRow) (bool, error) {
				viewCount++
				return false, wantErr
			})
			if !errors.Is(err, wantErr) || viewCount != 1 {
				t.Fatalf("view visitor ignored stop: %d, %v", viewCount, err)
			}
			err = VisitExecutionChanges(q, "p", 0, revision, true, 10, func(ExecutionChangeRow) (bool, error) {
				changeCount++
				return false, wantErr
			})
			if !errors.Is(err, wantErr) || changeCount != 1 {
				t.Fatalf("change visitor ignored stop: %d, %v", changeCount, err)
			}
		}
		traceCount := 0
		err = VisitTraceRuns(q, "p", 0, 0, 10, "", func(model.TraceRun) error {
			traceCount++
			return stop
		})
		if !errors.Is(err, stop) || traceCount != 1 {
			t.Fatalf("trace visitor ignored rejection: %d, %v", traceCount, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
