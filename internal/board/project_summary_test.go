package board

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Keep the previous /projects read path as a compatibility oracle and benchmark
// baseline, including the project ordering and graph hydration it performed.
func legacyGraphSummary(g Graph) Summary {
	s := Summary{Project: g.Project, FactCount: len(g.Facts), IntentCount: len(g.Intents), HintCount: len(g.Hints)}
	for _, i := range g.Intents {
		if i.ConcludedAt == nil {
			if i.Worker == nil {
				s.Unclaimed++
			} else {
				s.Working++
			}
		}
	}
	return s
}

func legacyProjectSummaries(tx *Tx) ([]Summary, error) {
	ids, err := tx.IDs()
	if err != nil {
		return nil, err
	}
	summaries := []Summary{}
	for _, id := range ids {
		graph, err := tx.Load(id)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, legacyGraphSummary(graph))
	}
	return summaries, nil
}

func assertProjectSummaries(t *testing.T, tx *Tx) []Summary {
	t.Helper()
	got, err := tx.ProjectSummaries()
	if err != nil {
		t.Fatal(err)
	}
	want, err := legacyProjectSummaries(tx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("project summaries differ from graph summaries:\ngot  %#v\nwant %#v", got, want)
	}
	return got
}

func TestProjectSummariesEmpty(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Do(context.Background(), func(tx *Tx) error {
		got := assertProjectSummaries(t, tx)
		if got == nil || len(got) != 0 {
			t.Fatalf("empty summaries must encode as [], got %#v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectSummariesPreserveCountsMetadataAndOrder(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Do(context.Background(), func(tx *Tx) error {
		for _, project := range []Project{
			{ID: "late", Title: "Late project", Status: "completed", CreatedAt: "2026-09-26T00:00:00Z"},
			{ID: "z-first-tie", Title: "First tie", Status: "terminated", Scenario: "audit", Bootstrap: true, CreatedAt: "2026-09-25T00:00:00Z", Reason: &Reason{Worker: "planner", Trigger: "changed", StartedAt: "started", Heartbeat: "heartbeat"}},
			{ID: "a-second-tie", Title: "Second tie", Status: "stopped", CreatedAt: "2026-09-25T00:00:00Z"},
			{ID: "early", Title: "Early project", Status: "active", CreatedAt: "2026-09-24T00:00:00Z"},
		} {
			g := Graph{Project: project}
			if project.ID == "z-first-tie" {
				g.Facts = []Fact{{ID: "f001", Description: "Evidence"}, {ID: "f002", Description: "More evidence"}}
				g.Hints = []Hint{{ID: "h001", Content: "Hint", Creator: "user", CreatedAt: tx.Now}}
				// Summary openness follows concluded_at, even for older rows whose
				// to_fact_id is inconsistent with it. An empty worker is still claimed.
				g.Intents = []Intent{
					{ID: "i001", Worker: Ptr("worker")},
					{ID: "i002", Worker: Ptr("")},
					{ID: "i003", Worker: Ptr("worker"), To: Ptr("f001")},
					{ID: "i004"},
					{ID: "i005", To: Ptr("f001")},
					{ID: "i006", Worker: Ptr("worker"), ConcludedAt: Ptr(tx.Now)},
					{ID: "i007", To: Ptr("f001"), ConcludedAt: Ptr(tx.Now)},
					{ID: "i008", ConcludedAt: Ptr("")},
				}
				for n := range g.Intents {
					g.Intents[n].Description = "Step"
					g.Intents[n].Creator = "fixture"
					g.Intents[n].CreatedAt = tx.Now
					g.Intents[n].From = []string{"f001", "f002"}
				}
			} else if project.ID == "a-second-tie" {
				// Same node IDs in another project must neither leak nor multiply
				// counts when the summary joins several one-to-many tables.
				g.Facts = []Fact{{ID: "f001", Description: "Other evidence"}}
				g.Intents = []Intent{{ID: "i001", Description: "Other step", Creator: "fixture", CreatedAt: tx.Now}}
				g.Hints = []Hint{{ID: "h001", Content: "Other hint", Creator: "user", CreatedAt: tx.Now}, {ID: "h002", Content: "Another hint", Creator: "user", CreatedAt: tx.Now}}
			}
			if err := tx.Save(g); err != nil {
				return err
			}
		}
		for _, query := range []string{
			"INSERT INTO pwnmesh_project_rounds(project_id,generation,restarted_at) VALUES('z-first-tie',3,'restarted')",
			"INSERT INTO pwnmesh_project_termination(project_id,terminated_at) VALUES('z-first-tie','terminated')",
			"UPDATE projects SET reason_worker='',reason_trigger=NULL,reason_started_at=NULL,reason_last_heartbeat_at=NULL WHERE id='a-second-tie'",
			"UPDATE projects SET reason_worker=NULL,reason_trigger='ignored',reason_started_at='ignored',reason_last_heartbeat_at='ignored' WHERE id='early'",
		} {
			if _, err := tx.Exec(query); err != nil {
				return err
			}
		}
		got := assertProjectSummaries(t, tx)
		if len(got) != 4 {
			t.Fatalf("project count=%d, want 4", len(got))
		}
		for n, id := range []string{"early", "z-first-tie", "a-second-tie", "late"} {
			if got[n].ID != id {
				t.Fatalf("position %d=%q, want %q (created_at then insertion order)", n, got[n].ID, id)
			}
		}
		main := got[1]
		if main.FactCount != 2 || main.IntentCount != 8 || main.Working != 3 || main.Unclaimed != 2 || main.HintCount != 1 {
			t.Fatalf("wrong mixed-state counts: %+v", main)
		}
		if main.Scenario != "audit" || main.Generation != 3 || main.RestartedAt != "restarted" || main.TerminatedAt != "terminated" || !main.Bootstrap || main.Reason == nil || main.Reason.Worker != "planner" {
			t.Fatalf("project metadata lost: %+v", main.Project)
		}
		if got[0].Reason != nil || got[2].Reason == nil || *got[2].Reason != (Reason{}) {
			t.Fatalf("NULL and empty reason workers were conflated: early=%+v second=%+v", got[0].Reason, got[2].Reason)
		}
		if got[2].FactCount != 1 || got[2].IntentCount != 1 || got[2].Working != 0 || got[2].Unclaimed != 1 || got[2].HintCount != 2 {
			t.Fatalf("other project's counts were mixed: %+v", got[2])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectSummariesSeeCurrentTransactionAndReopenedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Do(context.Background(), func(tx *Tx) error {
		if err := tx.Save(Graph{Project: Project{ID: "legacy", Title: "Legacy project", Status: "active", CreatedAt: tx.Now}, Facts: []Fact{{ID: "f001", Description: "Fact"}}, Intents: []Intent{{ID: "i001", Description: "Step", Creator: "fixture", CreatedAt: tx.Now}}}); err != nil {
			return err
		}
		before := assertProjectSummaries(t, tx)
		if len(before) != 1 || before[0].Unclaimed != 1 {
			t.Fatalf("new project is not visible inside the transaction: %+v", before)
		}
		for _, query := range []string{
			"UPDATE projects SET title='Updated title',status='stopped' WHERE id='legacy'",
			"UPDATE intents SET worker='worker' WHERE project_id='legacy'",
			"INSERT INTO hints(id,project_id,content,creator,created_at) VALUES('h001','legacy','Hint','user','now')",
			"DELETE FROM facts WHERE project_id='legacy'",
		} {
			if _, err := tx.Exec(query); err != nil {
				return err
			}
		}
		after := assertProjectSummaries(t, tx)
		if after[0].Title != "Updated title" || after[0].Status != "stopped" || after[0].FactCount != 0 || after[0].Working != 1 || after[0].Unclaimed != 0 || after[0].HintCount != 1 {
			t.Fatalf("summary missed uncommitted modifications: %+v", after[0])
		}
		return nil
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
	defer store.Close()
	if err := store.Do(context.Background(), func(tx *Tx) error {
		got := assertProjectSummaries(t, tx)
		if len(got) != 1 || got[0].Scenario != "" || got[0].Generation != 0 || got[0].RestartedAt != "" || got[0].TerminatedAt != "" {
			t.Fatalf("legacy project without optional metadata was omitted or changed: %+v", got)
		}
		if got[0].Working != 1 || got[0].HintCount != 1 || got[0].Title != "Updated title" {
			t.Fatalf("reopened summary lost committed data: %+v", got[0])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestObservedProjectSummariesMatchExpiryWithoutChangingStoredLeases(t *testing.T) {
	store := executionQueryStore(t)
	executionQueryTx(t, store, func(tx *Tx) {
		if err := tx.UpdateSettings(Settings{IntentTimeout: 30, ReasonTimeout: 60}); err != nil {
			t.Fatal(err)
		}
		now, err := time.Parse(time.RFC3339, tx.Now)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range []struct {
			id, heartbeat string
			version       int
		}{
			{"fresh", tx.Now, 1},
			{"expired", now.Add(-time.Hour).Format(time.RFC3339), 1},
			{"task-only-expired", now.Add(-45 * time.Second).Format(time.RFC3339), 1},
			{"invalid", "invalid-time", 1},
			{"missing", "", 1},
			{"legacy", now.Add(-time.Hour).Format(time.RFC3339), 0},
		} {
			heartbeat := Ptr(row.heartbeat)
			if row.id == "missing" {
				heartbeat = nil
			}
			lease := &Reason{Worker: "worker", Trigger: "initial", StartedAt: tx.Now, Heartbeat: row.heartbeat}
			graph := Graph{Project: Project{ID: row.id, Title: row.id, Status: "active", CreatedAt: tx.Now, OrchestrationVersion: row.version, Reason: lease, Curator: lease}, Intents: []Intent{
				{ID: "open", Worker: Ptr("worker"), Heartbeat: heartbeat, CreatedAt: tx.Now},
				{ID: "retained-result", To: Ptr("fact"), Worker: Ptr("worker"), Heartbeat: heartbeat, CreatedAt: tx.Now},
				{ID: "concluded", ConcludedAt: Ptr(tx.Now), Worker: Ptr("worker"), Heartbeat: heartbeat, CreatedAt: tx.Now},
				{ID: "unclaimed", CreatedAt: tx.Now},
			}}
			if row.version == 0 {
				graph.Project.Curator = nil
			}
			if err := tx.Save(graph); err != nil {
				t.Fatal(err)
			}
		}
		before := assertProjectSummaries(t, tx)
		observed, err := tx.ObservedProjectSummaries()
		if err != nil {
			t.Fatal(err)
		}
		if after := assertProjectSummaries(t, tx); !reflect.DeepEqual(before, after) {
			t.Fatal("observed summaries changed persisted leases")
		}
		if _, err := tx.Exec("SAVEPOINT summary_expiry"); err != nil {
			t.Fatal(err)
		}
		if err := tx.ExpireProject(""); err != nil {
			t.Fatal(err)
		}
		want := assertProjectSummaries(t, tx)
		if _, err := tx.Exec("ROLLBACK TO summary_expiry; RELEASE summary_expiry"); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(observed, want) {
			t.Fatalf("observed summaries differ from lease housekeeping:\ngot %+v\nwant %+v", observed, want)
		}
	})
}

var projectSummariesBenchmarkResult []Summary

func BenchmarkProjectSummaries(b *testing.B) {
	for _, size := range []struct{ projects, nodes, descriptionBytes int }{
		{1, 8, 128},
		{1, 128, 8192},
		{16, 8, 128},
		{16, 128, 8192},
	} {
		b.Run(fmt.Sprintf("projects=%d/nodes=%d/description=%d", size.projects, size.nodes, size.descriptionBytes), func(b *testing.B) {
			store, err := Open(":memory:")
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			description := strings.Repeat("x", size.descriptionBytes)
			if err := store.Do(context.Background(), func(tx *Tx) error {
				for project := 0; project < size.projects; project++ {
					g := Graph{Project: Project{ID: fmt.Sprintf("proj_%03d", project), Title: "Benchmark", Status: "active", Bootstrap: true, CreatedAt: tx.Now}}
					for node := 0; node < size.nodes; node++ {
						g.Facts = append(g.Facts, Fact{ID: fmt.Sprintf("f%03d", node), Description: description})
						intent := Intent{ID: fmt.Sprintf("i%03d", node), From: []string{"f000", "f001", "f002", "f003"}, Description: description, Creator: "fixture", CreatedAt: tx.Now}
						if node%3 == 1 {
							intent.Worker = Ptr("worker")
						} else if node%3 == 2 {
							intent.To, intent.ConcludedAt = Ptr("f000"), Ptr(tx.Now)
						}
						g.Intents = append(g.Intents, intent)
					}
					for hint := 0; hint < 4; hint++ {
						g.Hints = append(g.Hints, Hint{ID: fmt.Sprintf("h%03d", hint), Content: description, Creator: "user", CreatedAt: tx.Now})
					}
					if err := tx.Save(g); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			for _, read := range []struct {
				name string
				fn   func(*Tx) ([]Summary, error)
			}{{"legacy", legacyProjectSummaries}, {"aggregate", (*Tx).ProjectSummaries}} {
				b.Run(read.name, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					for n := 0; n < b.N; n++ {
						// Each iteration is a read-only transaction over a fixed fixture;
						// graph creation and payload allocation are excluded above.
						if err := store.Do(context.Background(), func(tx *Tx) error {
							var err error
							projectSummariesBenchmarkResult, err = read.fn(tx)
							return err
						}); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
