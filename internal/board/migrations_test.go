package board

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreMigrationBaselineAndReopenMaintenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "versioned.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()
	var appliedAt string
	if err := store.Do(context.Background(), func(tx *Tx) error {
		var version int
		var name string
		if err := tx.transactionQueries.QueryRow("SELECT version,name,applied_at FROM pwnmesh_schema_migrations WHERE version=4").Scan(&version, &name, &appliedAt); err != nil {
			return err
		}
		if version != 4 || name != "normalized-state-records" || appliedAt == "" {
			t.Fatalf("unexpected baseline: %d %q %q", version, name, appliedAt)
		}
		// Retained/imported identities must still repair counters on later opens.
		if err := tx.Save(Graph{
			Project: Project{ID: "proj_041", Title: "Imported", Status: "active", CreatedAt: tx.Now},
			Facts:   []Fact{{ID: "f012", Description: "Imported observation"}},
		}); err != nil {
			return err
		}
		putQueryExecution(t, tx, Execution{ProjectID: "proj_041", ID: "restored", Status: "succeeded"}, 7, "")
		_, err := tx.transactionQueries.Exec(`DELETE FROM counters WHERE name='project'; DELETE FROM scoped_counters;
DELETE FROM settings; DELETE FROM pwnmesh_installation;
DELETE FROM pwnmesh_ui_execution_changes; DELETE FROM pwnmesh_ui_execution_revisions;`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Do(context.Background(), func(tx *Tx) error {
		var count int
		var after string
		if err := tx.transactionQueries.QueryRow("SELECT COUNT(*),MAX(applied_at) FROM pwnmesh_schema_migrations").Scan(&count, &after); err != nil {
			return err
		}
		if count != 4 || after != appliedAt {
			t.Fatalf("reopen rewrote migration history: count=%d before=%q after=%q", count, appliedAt, after)
		}
		if id, err := tx.Next("", "project"); err != nil || id != "proj_042" {
			t.Fatalf("versioned reopen did not repair project counter: %q %v", id, err)
		}
		if id, err := tx.Next("proj_041", "fact"); err != nil || id != "f013" {
			t.Fatalf("versioned reopen did not repair fact counter: %q %v", id, err)
		}
		var generation, metadataVersion int
		if err := tx.transactionQueries.QueryRow("SELECT generation,metadata_version FROM pwnmesh_executions WHERE id='restored'").Scan(&generation, &metadataVersion); err != nil || generation != 7 || metadataVersion != 1 {
			t.Fatalf("versioned reopen changed execution metadata: generation=%d version=%d err=%v", generation, metadataVersion, err)
		}
		settings, err := tx.Settings()
		if err != nil || settings.IntentTimeout != 15 || settings.ReasonTimeout != 15 {
			t.Fatalf("versioned reopen did not restore default settings: %+v %v", settings, err)
		}
		var identity string
		if err := tx.transactionQueries.QueryRow("SELECT identity FROM pwnmesh_installation WHERE singleton=1").Scan(&identity); err != nil || len(identity) != 32 {
			t.Fatalf("versioned reopen did not reseed missing installation: %q %v", identity, err)
		}
		if err := tx.transactionQueries.QueryRow("SELECT COUNT(*) FROM pwnmesh_ui_execution_changes c JOIN pwnmesh_ui_execution_revisions r ON c.project_id=r.project_id WHERE c.execution_id='restored' AND c.revision<=r.revision").Scan(&count); err != nil || count != 1 {
			t.Fatalf("versioned reopen did not restore execution change projection: %d %v", count, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertStoreRejectedUnchanged(t *testing.T, path, message string) {
	t.Helper()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if store != nil {
		store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("unsupported store was not rejected: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected store changed database bytes: %v", err)
	}
}

func TestStoreRejectsUnsupportedExecutionMetadataWithoutBackfill(t *testing.T) {
	for _, version := range []int{0, 2} {
		path := filepath.Join(t.TempDir(), "metadata.db")
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		executionQueryTx(t, store, func(tx *Tx) {
			putQueryExecution(t, tx, Execution{ID: "retained", Status: "succeeded"}, 7, "")
			if _, err := tx.transactionQueries.Exec("UPDATE pwnmesh_executions SET metadata_version=?; DELETE FROM settings", version); err != nil {
				t.Fatal(err)
			}
		})
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		assertStoreRejectedUnchanged(t, path, "unsupported execution metadata version")
	}
}

func TestDomainReadsCurrentStoreAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.transactionQueries.Exec(`
INSERT INTO projects(id,title,status,created_at) VALUES('proj_041','Retained','active','then');
INSERT INTO facts VALUES('f012','proj_041','Retained observation');
INSERT INTO pwnmesh_project_metadata VALUES('proj_041','ctf');
INSERT INTO pwnmesh_project_orchestration VALUES('proj_041',1,'curator@retained','input','then','then');`)
		return err
	})
	closeErr := store.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("create current fixture: exec=%v close=%v", err, closeErr)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Do(context.Background(), func(tx *Tx) error {
		g, err := tx.Load("proj_041")
		if err != nil {
			return err
		}
		if g.Project.Title != "Retained" || g.Project.Scenario != "ctf" || g.Project.OrchestrationVersion != 1 || g.Project.Curator == nil || g.Project.Curator.Worker != "curator@retained" || len(g.Facts) != 1 || g.Facts[0].ID != "f012" {
			t.Fatalf("domain graph lost retained values: %+v", g)
		}
		if id, err := tx.Next("proj_041", "fact"); err != nil || id != "f013" {
			t.Fatalf("reopened counter = %q, %v", id, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
