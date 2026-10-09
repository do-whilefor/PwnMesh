package board

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Measure a real execution boundary against an unchanged graph. No snapshot
// survives a transaction, and setup/serialization are outside the measurement.
func BenchmarkCheckExecutionDependencies(b *testing.B) {
	for _, count := range []int{16, 512} {
		b.Run(fmt.Sprintf("steps=%d", count), func(b *testing.B) {
			store, err := Open(filepath.Join(b.TempDir(), "dependencies.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			if err := store.Do(context.Background(), func(tx *Tx) error {
				graph := Graph{Project: Project{ID: "p", Title: "dependency benchmark", Status: "active", OrchestrationVersion: 1, CreatedAt: tx.Now}, Facts: []Fact{{ID: "origin", Description: "Local fixture"}, {ID: "goal", Description: "Observe fixture"}}}
				for n := range count {
					graph.Intents = append(graph.Intents, Intent{ID: fmt.Sprintf("i%04d", n), From: []string{"origin"}, Description: "Observe independent fixture", Creator: "planner", CreatedAt: tx.Now})
				}
				return tx.Save(graph)
			}); err != nil {
				b.Fatal(err)
			}
			execution := Execution{ProjectID: "p", Kind: "explore", Intent: "i0000", Job: json.RawMessage(`{}`)}
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				if err := store.Do(context.Background(), func(tx *Tx) error {
					return tx.CheckExecutionDependencies(execution)
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkStepDependencyResultsArchivedBodies(b *testing.B) {
	for _, size := range []int{128, 1 << 20} {
		b.Run(fmt.Sprintf("body_bytes=%d", size), func(b *testing.B) {
			f := newOrchestrationFixture(b)
			producer := f.worker("producer", "")
			fact := f.fact(producer, "accepted")
			f.finish(producer, fact)
			child := dependentStep(f, "consumer", producer.Intent)
			// Retained model inputs and transcripts grow independently of the
			// accepted Step/Fact/Run binding used by downstream execution.
			body, err := json.Marshal(map[string]string{"archive": strings.Repeat("x", size)})
			if err != nil {
				b.Fatal(err)
			}
			f.do(func(tx *Tx) error {
				_, err := tx.Exec("UPDATE xloom_executions SET job=?,result=? WHERE project_id=? AND id=?", body, body, producer.ProjectID, producer.ID)
				return err
			})
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				f.do(func(tx *Tx) error {
					results, err := tx.StepDependencyResults("p", child)
					if err == nil && (len(results) != 1 || results[0] != (DependencyResult{StepID: producer.Intent, FactID: fact, RunID: producer.ID})) {
						b.Fatalf("wrong successful binding: %+v", results)
					}
					return err
				})
			}
		})
	}
}
