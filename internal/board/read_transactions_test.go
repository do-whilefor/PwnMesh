package board

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestViewKeepsConsistentSnapshotWithoutBlockingWriter(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "views.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Do(context.Background(), func(tx *Tx) error {
		return tx.Save(Graph{Project: Project{ID: "p", Title: "before", Status: "active", CreatedAt: tx.Now}})
	}); err != nil {
		t.Fatal(err)
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- store.View(context.Background(), func(tx *Tx) error {
			first, err := tx.Load("p")
			close(started)
			if err != nil {
				return err
			}
			<-release
			second, err := tx.Load("p")
			if err == nil && (first.Project.Title != "before" || second.Project.Title != "before") {
				t.Error("one View observed two committed state versions")
			}
			return err
		})
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = store.Do(ctx, func(tx *Tx) error {
		_, err := tx.Exec("UPDATE projects SET title='after' WHERE id='p'")
		return err
	})
	close(release)
	if readErr := <-done; readErr != nil {
		t.Fatal(readErr)
	}
	if err != nil {
		t.Fatalf("observational read blocked a writer: %v", err)
	}
	if err := store.View(context.Background(), func(tx *Tx) error {
		got, err := tx.Load("p")
		if err == nil && got.Project.Title != "after" {
			t.Fatal("new View did not observe the committed update")
		}
		if _, writeErr := tx.Exec("UPDATE projects SET title='forbidden' WHERE id='p'"); writeErr == nil {
			t.Fatal("query-only View accepted a mutation")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryViewUsesTheExistingDatabase(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.View(context.Background(), func(tx *Tx) error {
		_, err := tx.Settings()
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
