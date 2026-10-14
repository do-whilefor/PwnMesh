package board

import (
	"context"
	"testing"
)

func TestGraphSaveRejectsCuratorBeforePersistence(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Do(context.Background(), func(tx *Tx) error {
		graph := Graph{Project: Project{ID: "rejected", Title: "Invalid curator", Status: "active", CreatedAt: tx.Now, Curator: &Reason{Worker: "curator"}}}
		requireAPIStatus(t, tx.Save(graph), 422)
		// A handled validation error must not leave half of a graph behind.
		requireAPIStatus(t, tx.RequireProject(graph.Project.ID), 404)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
