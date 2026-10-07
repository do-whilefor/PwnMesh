package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"pwnmesh/internal/board"
)

func TestProjectRequestsExpireOnlyTheirOwnLeases(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	old := store.Now().Add(-time.Hour).Format(time.RFC3339)
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		return tx.Save(board.Graph{Project: board.Project{ID: "unrelated", Title: "Unrelated", Status: "active", OrchestrationVersion: 1, CreatedAt: old,
			Reason: &board.Reason{Worker: "crashed", Trigger: "initial", StartedAt: old, Heartbeat: old}}})
	}); err != nil {
		t.Fatal(err)
	}
	f.request("GET", f.base(), nil, false, http.StatusOK, nil)
	check := func(expired bool) {
		if err := store.Do(context.Background(), func(tx *board.Tx) error {
			g, err := tx.Load("unrelated")
			if err == nil && (g.Project.Reason == nil) != expired {
				t.Fatalf("unrelated lease expired=%v after scoped request, expected %v", g.Project.Reason == nil, expired)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	check(false)
	// The dispatcher's global project listing still clears all crashed leases.
	f.request("GET", "/projects", nil, false, http.StatusOK, nil)
	check(true)
}

// Measure one live heartbeat while unrelated projects retain completed work.
// Fixture construction and model/backend execution are outside the timer.
func BenchmarkHeartbeatProjectHistory(b *testing.B) {
	for _, projects := range []int{1, 128} {
		b.Run(fmt.Sprintf("projects=%d", projects), func(b *testing.B) {
			store, err := board.Open(filepath.Join(b.TempDir(), "history.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			store.Now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
			if err := store.Do(context.Background(), func(tx *board.Tx) error {
				for p := 0; p < projects; p++ {
					g := board.Graph{Project: board.Project{ID: fmt.Sprintf("p%d", p), Title: "History", Status: "active", OrchestrationVersion: 1, CreatedAt: tx.Now,
						Reason: &board.Reason{Worker: "planner", Trigger: "initial", StartedAt: tx.Now, Heartbeat: tx.Now}}}
					g.Facts = []board.Fact{{ID: "origin", Description: "Input"}, {ID: "goal", Description: "Goal"}}
					for n := 0; n < 32; n++ {
						fact := fmt.Sprintf("f%d", n)
						g.Facts = append(g.Facts, board.Fact{ID: fact, Description: "Retained evidence"})
						g.Intents = append(g.Intents, board.Intent{ID: fmt.Sprintf("i%d", n), From: []string{"origin"}, To: &fact,
							Description: "Completed task", Creator: "planner", Worker: board.Ptr("finished"), Heartbeat: board.Ptr(tx.Now), CreatedAt: tx.Now, ConcludedAt: board.Ptr(tx.Now)})
						g.Hints = append(g.Hints, board.Hint{ID: fmt.Sprintf("h%d", n), Content: "Retained hint", Creator: "user", CreatedAt: tx.Now})
					}
					if err := tx.Save(g); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			handler := New(store)
			body := []byte(`{"worker":"planner"}`)
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest("POST", "/projects/p0/reason/heartbeat", bytes.NewReader(body)))
				if response.Code != http.StatusOK {
					b.Fatalf("heartbeat: %d %s", response.Code, response.Body.String())
				}
			}
		})
	}
}
