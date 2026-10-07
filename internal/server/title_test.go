package server

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board"
)

func TestTitleUpdatesOnlyTitleAndPreservesState(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.newIntent()
	f.request("POST", f.base()+"/hints", map[string]string{"content": "Keep this requirement", "creator": "user"}, false, http.StatusCreated, nil)
	before, events := f.state(), storedEvents(f)
	version := board.DecisionStateVersion(before)
	if err := f.store.Do(context.Background(), func(tx *board.Tx) error {
		for _, table := range []string{"facts", "hints", "intents", "intent_sources", "xloom_project_orchestration"} {
			if _, err := tx.Exec("CREATE TRIGGER reject_title_insert_" + table + " BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT,'title rewrote graph'); END"); err != nil {
				return err
			}
		}
		for _, table := range []string{"facts", "hints", "intents", "intent_sources", "xloom_state"} {
			if _, err := tx.Exec("CREATE TRIGGER reject_title_update_" + table + " BEFORE UPDATE ON " + table + " BEGIN SELECT RAISE(ABORT,'title rewrote graph'); END"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var project board.Project
	f.request("PUT", f.base()+"/title", map[string]string{"title": "  Renamed task  "}, false, http.StatusOK, &project)
	before.Graph.Project.Title = "Renamed task"
	after := f.state()
	if !reflect.DeepEqual(after, before) || !reflect.DeepEqual(project, after.Graph.Project) || !reflect.DeepEqual(storedEvents(f), events) {
		t.Fatal("renaming changed other project data or event history")
	}
	if board.DecisionStateVersion(after) == version {
		t.Fatal("renaming did not invalidate the prior decision input")
	}
	for _, title := range []any{" ", nil, 42} {
		response := f.request("PUT", f.base()+"/title", map[string]any{"title": title}, false, http.StatusUnprocessableEntity, nil)
		if !strings.Contains(response, "must be a non-empty string") || !reflect.DeepEqual(f.state(), after) {
			t.Fatal("invalid title lost its validation error or changed the project")
		}
	}
	f.request("PUT", "/projects/missing/title", map[string]string{"title": " "}, false, http.StatusNotFound, nil)
}
