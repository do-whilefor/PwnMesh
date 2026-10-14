package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"pwnmesh/internal/board/model"
)

func TestGraphMutationsPreserveImmutableFieldsAndProjectScope(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Do(context.Background(), func(q Queries) error {
		for _, project := range []string{"p", "other"} {
			if err := SaveGraph(q, leaseGraph(project, 1)); err != nil {
				return err
			}
		}
		for n, description := range []string{"original", "replacement"} {
			inserted, err := InsertFact(q, "p", model.Fact{ID: "observed", Description: description})
			if err != nil || inserted != (n == 0) {
				t.Fatalf("immutable fact insert: %v, %v", inserted, err)
			}
		}
		if _, err := q.Exec(`CREATE TRIGGER reject_fact BEFORE INSERT ON facts WHEN NEW.id='rejected' BEGIN SELECT RAISE(ABORT,'fact rejected'); END`); err != nil {
			return err
		}
		if inserted, err := InsertFact(q, "p", model.Fact{ID: "rejected"}); err == nil || inserted {
			t.Fatal("fact insert swallowed a non-collision failure")
		}
		if err := AbandonIntent(q, "p", "open", "abandoned"); err != nil {
			return err
		}
		if err := RevokeRun(q, "p", "worker"); err != nil {
			return err
		}
		graph, err := LoadGraph(q, "p")
		if err != nil {
			return err
		}
		want := leaseGraph("p", 1)
		want.Facts = append(want.Facts, model.Fact{ID: "observed", Description: "original"})
		want.Intents[0].Worker, want.Intents[0].ConcludedAt = nil, model.Ptr("abandoned")
		if !reflect.DeepEqual(graph, want) {
			t.Fatal("abandon changed unrelated graph fields")
		}
		for _, project := range []string{"p", "other"} {
			revoked, err := RunRevoked(q, project, "worker")
			if err != nil || revoked != (project == "p") {
				t.Fatalf("revocation crossed project: %s, %v, %v", project, revoked, err)
			}
		}
		conclusion := model.Intent{ID: "open", To: model.Ptr("goal"), Worker: model.Ptr("next"), Heartbeat: model.Ptr("heartbeat"), ConcludedAt: model.Ptr("concluded"), Description: "ignored", Creator: "ignored", From: []string{"ignored"}, CreatedAt: "ignored"}
		if err := SaveIntentConclusion(q, "p", conclusion); err != nil {
			return err
		}
		want.Intents[0].To, want.Intents[0].Worker = conclusion.To, conclusion.Worker
		want.Intents[0].Heartbeat, want.Intents[0].ConcludedAt = conclusion.Heartbeat, conclusion.ConcludedAt
		graph, err = LoadGraph(q, "p")
		if err != nil || !reflect.DeepEqual(graph, want) {
			t.Fatalf("conclusion rewrote immutable fields: %v", err)
		}
		if err := DeleteIntent(q, "p", "open"); err != nil {
			return err
		}
		graph, err = LoadGraph(q, "other")
		if err != nil || !reflect.DeepEqual(graph, leaseGraph("other", 1)) {
			t.Fatalf("targeted mutation changed another project: %v", err)
		}
		var count int
		if err := q.QueryRow("SELECT COUNT(*) FROM intents WHERE project_id='p' AND id='open'").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("intent deletion retained the target")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGraphSequencesAndAssetsFollowCallerRollback(t *testing.T) {
	store := stateEventStore(t, ":memory:")
	ctx := context.Background()
	asset := model.Asset{ID: "asset", Kind: "url", Value: "https://fixture.invalid", Method: "GET"}
	rollback := errors.New("reject combined mutation")
	for _, reject := range []bool{true, false} {
		err := store.Do(ctx, func(q Queries) error {
			for _, allocation := range []struct {
				project, kind string
				want          int
			}{{"p", "goal", 1}, {"other", "goal", 1}, {"p", "goal", 2}, {"p", "candidate", 1}} {
				id, err := NextScopedID(q, allocation.project, allocation.kind, allocation.kind)
				if err != nil || id != fmt.Sprintf("%s%03d", allocation.kind, allocation.want) {
					t.Fatalf("sequence lost scope or rollback: %s, %v", id, err)
				}
			}
			for _, generation := range []int64{0, 0, 1} {
				if err := InsertAssetAnchors(q, "p", generation, "fact", "observed", []model.Asset{asset}); err != nil {
					return err
				}
			}
			changed := asset
			changed.Value = "must not replace"
			if err := InsertAssetAnchors(q, "p", 0, "finding", "finding", []model.Asset{changed}); err != nil {
				return err
			}
			assets, anchors, err := ProjectAssets(q, "p", 0)
			if err != nil || !reflect.DeepEqual(assets, []model.Asset{asset}) || len(anchors) != 2 {
				t.Fatalf("asset upsert changed bytes or duplicated anchor: %v %v %v", assets, anchors, err)
			}
			if reject {
				return rollback
			}
			return nil
		})
		if reject && !errors.Is(err, rollback) || !reject && err != nil {
			t.Fatalf("caller transaction: %v", err)
		}
		if reject {
			if err := store.View(ctx, func(q Queries) error {
				assets, anchors, err := ProjectAssets(q, "p", 0)
				if len(assets) != 0 || len(anchors) != 0 {
					t.Fatal("asset storage committed independently")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
