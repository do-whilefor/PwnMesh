package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestGraphRowsPreserveIdentityOrderAndImmutableInputs(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	graph := model.Graph{
		Project: model.Project{ID: "project", Title: "Initial title", Status: "active", CreatedAt: "created", Scenario: "audit", OrchestrationVersion: 1},
		Facts:   []model.Fact{{ID: "origin", Description: "Original scope"}, {ID: "proof", Description: "Original observation"}},
		Hints:   []model.Hint{{ID: "second", Content: "Original hint", Creator: "user", CreatedAt: "created"}, {ID: "first", Content: "Next hint", Creator: "user", CreatedAt: "created"}},
		Intents: []model.Intent{{ID: "step", From: []string{"proof", "origin"}, Description: "Original task", Creator: "planner", CreatedAt: "created"}},
	}
	ctx := context.Background()
	if err := store.Do(ctx, func(q Queries) error { return SaveGraph(q, graph) }); err != nil {
		t.Fatal(err)
	}
	graph.Project.Title, graph.Project.CreatedAt, graph.Project.Scenario = "Updated title", "changed", ""
	graph.Facts[0].Description = "Changed scope"
	graph.Hints[0].Content = "Changed hint"
	graph.Intents[0].Description = "Changed task"
	if err := store.Do(ctx, func(q Queries) error { return SaveGraph(q, graph) }); err != nil {
		t.Fatal(err)
	}
	if err := store.View(ctx, func(q Queries) error {
		got, err := LoadGraph(q, graph.Project.ID)
		if err != nil {
			return err
		}
		if got.Project.Title != "Updated title" || got.Project.CreatedAt != "created" || got.Project.Scenario != "audit" || got.Project.Reason != nil || got.Project.Curator != nil {
			t.Fatalf("project persistence changed identity, omitted metadata or null leases: %+v", got.Project)
		}
		if got.Facts[0].Description != "Original scope" || got.Hints[0].ID != "second" || got.Hints[0].Content != "Original hint" || got.Intents[0].Description != "Original task" || !reflect.DeepEqual(got.Intents[0].From, []string{"proof", "origin"}) {
			t.Fatalf("graph upsert rewrote original inputs or insertion order: %+v", got)
		}
		_, err = LoadGraph(q, "missing")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("storage must return its missing-row error for Board to interpret: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rejected := errors.New("outer operation rejected")
	if err := store.Do(ctx, func(q Queries) error {
		graph.Project.ID = "rolled-back"
		if err := SaveGraph(q, graph); err != nil {
			return err
		}
		return rejected
	}); !errors.Is(err, rejected) {
		t.Fatalf("outer transaction error = %v", err)
	}
	if err := store.View(ctx, func(q Queries) error {
		exists, err := ProjectExists(q, "rolled-back")
		if exists {
			t.Fatal("graph repository committed independently of its caller")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
