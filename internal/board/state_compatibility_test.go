package board

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"pwnmesh/internal/board/internal/sqlstore"
)

func TestNormalizedStatePreservesCanonicalInput(t *testing.T) {
	for _, metadata := range []string{
		`{"goals":null,"steps":[],"facts":null,"findings":[],"fact_relations":null,"candidates":[],"disputes":null}`,
		`{"goals":[{"id":"g1","parent_id":"goal","condition":"Check observation","status":"open","sources":["f1"]}],
		"steps":[{"id":"i1","goal_id":"g1","priority":3,"depends_on":[],"write_paths":["/workspace/result"]},{"id":"i1","goal_id":"g1","priority":7},{"id":"orphan","status":"abandoned"}],
		"facts":[{"id":"f1","description":"Observed","status":"valid","legacy":true,"evidence":[]}],
		"findings":[{"id":"finding1","claim":"Retained claim","scope":"fixture","status":"candidate","sources":["f1"],"evidence":[]}],
		"fact_relations":[{"kind":"narrows","source":"f1","target":"f1","reason":"Retained relation"}],
		"candidates":[{"id":"c1","claim":"Candidate claim","status":"candidate","sources":["f1"],"evidence":[],"generation":0,"revision":2}],
		"disputes":[{"id":"d1","finding_id":"finding1","candidate_ids":["c1"],"status":"open","review_step_ids":["i1"]}],
		"curation":{"generation":0,"through_revision":1,"request":{"sources":["f1"],"reason":"Reconcile","revision":2}}}`,
	} {
		path := filepath.Join(t.TempDir(), "canonical.db")
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		var before []byte
		var version, snapshotID string
		if err := store.Do(context.Background(), func(tx *Tx) error {
			graph := Graph{
				Project: Project{ID: "p", Title: "Canonical input", Status: "active", CreatedAt: "then"},
				Facts:   []Fact{{ID: "origin", Description: "Input"}, {ID: "goal", Description: "Goal"}, {ID: "f1", Description: "Observed"}},
				Intents: []Intent{{ID: "i1", From: []string{"f1"}, Description: "Inspect", Creator: "planner", CreatedAt: "then"}},
			}
			if err := tx.Save(graph); err != nil {
				return err
			}
			graph, err := tx.Load("p")
			if err != nil {
				return err
			}
			var data stateData
			if err := json.Unmarshal([]byte(metadata), &data); err != nil {
				return err
			}
			state, err := tx.projectState(graph, data, 4, 2)
			if err != nil {
				return err
			}
			before, err = json.Marshal(state)
			if err != nil {
				return err
			}
			version = DecisionStateVersion(state)
			ref, err := tx.FreezeInput(state)
			if err != nil {
				return err
			}
			snapshotID = ref.ID
			return sqlstore.SaveStateMetadata(tx.transactionQueries, "p", metadata, 4, 2)
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
			state, err := tx.State("p")
			if err != nil {
				return err
			}
			after, err := json.Marshal(state)
			if err != nil {
				return err
			}
			if !bytes.Equal(before, after) || DecisionStateVersion(state) != version {
				t.Fatalf("normalization changed canonical input: before=%s after=%s", before, after)
			}
			ref, err := tx.FreezeInput(state)
			if err != nil {
				return err
			}
			if ref.ID != snapshotID {
				t.Fatalf("normalization changed frozen input identity: %s != %s", ref.ID, snapshotID)
			}
			frozen, err := tx.readInputSnapshotBytes("p", snapshotID)
			if err == nil && !bytes.Equal(frozen, before) {
				t.Fatal("normalization rewrote retained snapshot bytes")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDecisionRetryKeyPreservesCurrentBytes(t *testing.T) {
	g := Graph{Project: Project{ID: "fixture", OrchestrationVersion: 1}}
	if got := DecisionRetryKey(g, 0); got != "reason:6131a7e2e21f30d058650b89faec6a12380760e4c8ef0d39b481400bb08c2ac0" {
		t.Fatalf("nil collection identity changed: %s", got)
	}
	g.Facts, g.Hints = []Fact{}, []Hint{}
	g.Intents = []Intent{{ID: "open"}, {ID: "done", To: Ptr("fact")}, {ID: "abandoned", ConcludedAt: Ptr("now")}}
	if got := DecisionRetryKey(g, 7); got != "reason:a86d0bc22e11e5da882fdc78a134a95cafd08467213a711605afcc547492a35d" {
		t.Fatalf("ordered completion identity changed: %s", got)
	}
}

func TestCompactStepMetadataLifecycleAndReopen(t *testing.T) {
	f := newPlanFixture(t)
	id := f.action("step", "add", stepInput("Current task", []string{"f002", "f001"}, "goal", 7)).ID
	check := func(status string, priority int, reason string) {
		t.Helper()
		state := f.state()
		step := state.Steps[0]
		if step.ID != id || step.Status != status || step.Priority != priority || step.Reason != reason || step.Description != "Current task" || !reflect.DeepEqual(step.From, []string{"f002", "f001"}) {
			t.Fatalf("unexpected Step projection: %+v", step)
		}
		f.tx(func(tx *Tx) error {
			raw, _, _, err := sqlstore.StateMetadata(tx.transactionQueries, "proj_001")
			if err != nil {
				return err
			}
			var stored struct {
				Steps []map[string]json.RawMessage `json:"steps"`
			}
			if err := json.Unmarshal(raw, &stored); err != nil {
				return err
			}
			for key := range stored.Steps[0] {
				if key != "id" && key != "goal_id" && key != "priority" && key != "reason" && key != "status" {
					t.Fatalf("duplicate Step field persisted: %s", key)
				}
			}
			if marker := string(stored.Steps[0]["status"]); marker != "" && marker != `"abandoned"` {
				t.Fatalf("runtime status persisted: %s", marker)
			}
			// Storage reconstruction preserves the metadata's public Step fields;
			// runtime fields still come from the authoritative Intent.
			var decoded struct {
				Steps []Step `json:"steps"`
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return err
			}
			storedStep := decoded.Steps[0]
			if storedStep.ID != step.ID || storedStep.GoalID != step.GoalID || storedStep.Priority != step.Priority || storedStep.Reason != step.Reason || (storedStep.Status == "abandoned") != (status == "abandoned") {
				t.Fatalf("reconstructed metadata lost Step fields: %+v", storedStep)
			}
			return nil
		})
	}
	check("open", 7, "")
	f.action("step", "priority", map[string]any{"action": "priority", "id": id, "priority": 19, "reason": "Prioritize observation"})
	check("open", 19, "Prioritize observation")
	f.tx(func(tx *Tx) error {
		_, err := tx.transactionQueries.Exec("UPDATE intents SET worker='runner@current',last_heartbeat_at=? WHERE project_id='proj_001' AND id=?", tx.Now, id)
		return err
	})
	check("running", 19, "Prioritize observation")
	f.action("step", "abandon", map[string]any{"action": "abandon", "id": id, "reason": "No longer needed"})
	check("abandoned", 19, "No longer needed")
	completedID := f.action("step", "add-completed", stepInput("Second task", []string{"origin"}, "goal", 3)).ID
	var databasePath string
	f.tx(func(tx *Tx) error {
		if _, err := tx.transactionQueries.Exec("UPDATE intents SET worker='runner@completed',to_fact_id='f001',concluded_at=? WHERE project_id='proj_001' AND id=?", tx.Now, completedID); err != nil {
			return err
		}
		var sequence int
		var name string
		return tx.transactionQueries.QueryRow("PRAGMA database_list").Scan(&sequence, &name, &databasePath)
	})
	before := f.state()
	if before.Steps[1].Status != "completed" || Value(before.Steps[1].Result) != "f001" || Value(before.Steps[1].Worker) != "runner@completed" {
		t.Fatalf("completed projection: %+v", before.Steps[1])
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	f.store = reopened
	if after := f.state(); !reflect.DeepEqual(before, after) {
		t.Fatalf("reopen changed projection: before=%+v after=%+v", before, after)
	}
}

func TestNormalizedFactProjectionPreservesOrderAndRelationPrecedence(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "facts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = store.Do(context.Background(), func(tx *Tx) error {
		graph := Graph{
			Project: Project{ID: "observations", Title: "Ordered observations", Status: "active", CreatedAt: tx.Now},
			Facts: []Fact{{ID: "origin", Description: "Original requirement"}, {ID: "goal", Description: "Root goal"},
				{ID: "f001", Description: "First graph fact"}, {ID: "f002", Description: "Second graph fact"}, {ID: "f003", Description: "Legacy fact"}},
		}
		if err := tx.Save(graph); err != nil {
			return err
		}
		const raw = `{"facts":[{"id":"f002","description":"Second observation","status":"valid"},{"id":"f001","description":"First observation","status":"valid"},{"id":"f001","description":"Duplicate must not replace the first","status":"refuted"},{"id":"missing","description":"Unlinked record","status":"valid"}],"fact_relations":[{"kind":"refutes","target":"f001","source":"f002"},{"kind":"narrows","target":"f001","source":"f002"}]}`
		if err := sqlstore.SaveStateMetadata(tx.transactionQueries, "observations", raw, 4, 2); err != nil {
			return err
		}
		var header string
		if err := tx.transactionQueries.QueryRow("SELECT data FROM pwnmesh_state WHERE project_id='observations'").Scan(&header); err != nil {
			return err
		}
		state, err := tx.State("observations")
		if err != nil {
			return err
		}
		want := []FactRecord{
			{ID: "origin", Description: "Original requirement", Status: "input", Evidence: []EvidenceRef{}, Legacy: true},
			{ID: "goal", Description: "Root goal", Status: "input", Evidence: []EvidenceRef{}, Legacy: true},
			{ID: "f001", Description: "First observation", Status: "narrowed"},
			{ID: "f002", Description: "Second observation", Status: "valid"},
			{ID: "f003", Description: "Legacy fact", Status: "valid", Evidence: []EvidenceRef{}, Legacy: true},
		}
		if !reflect.DeepEqual(state.FactRecords, want) || !reflect.DeepEqual(state.Graph.Facts, graph.Facts) {
			t.Fatalf("normalized fact projection changed: %+v", state.FactRecords)
		}
		var saved string
		if err := tx.transactionQueries.QueryRow("SELECT data FROM pwnmesh_state WHERE project_id='observations'").Scan(&saved); err != nil {
			return err
		}
		if saved != header || state.Revision != 4 || state.DecisionRevision != 2 {
			t.Fatal("reading normalized facts changed their retained data or revisions")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNormalizedFactMatchesProjectionAndFrozenInput(t *testing.T) {
	f := newOrchestrationFixture(t)
	e := f.worker("observation", "")
	id := f.fact(e, "current-observation")
	before := f.state()
	version := DecisionStateVersion(before)
	var ref *InputSnapshot
	f.do(func(tx *Tx) error {
		raw, _, _, err := sqlstore.StateMetadata(tx.transactionQueries, "p")
		if err != nil {
			return err
		}
		// The storage reader combines normalized observations with metadata.
		var decoded struct {
			Facts []FactRecord `json:"facts"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return err
		}
		var base Fact
		if err := tx.transactionQueries.QueryRow("SELECT id,description FROM facts WHERE project_id='p' AND id=?", id).Scan(&base.ID, &base.Description); err != nil {
			return err
		}
		if len(decoded.Facts) != 1 || decoded.Facts[0].ID != base.ID || decoded.Facts[0].Description != base.Description || !reflect.DeepEqual(decoded.Facts[0], before.FactRecords[len(before.FactRecords)-1]) {
			t.Fatal("current Fact, metadata reader and SQL base do not describe the same observation")
		}
		ref, err = tx.FreezeInput(before)
		return err
	})
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	if after := f.state(); !reflect.DeepEqual(before, after) || DecisionStateVersion(after) != version {
		t.Fatal("reopening changed the Fact projection or decision identity")
	}
	f.do(func(tx *Tx) error {
		frozen, err := tx.ReadInputSnapshot("p", ref.ID)
		if err == nil && (!reflect.DeepEqual(before, frozen) || DecisionStateVersion(frozen) != version) {
			t.Fatal("frozen Fact input changed after reopen")
		}
		return err
	})
}

// Retained projects can still be inspected, but cannot revive retired writes.
func TestRetiredProjectIsReadableWithoutMutationProtocols(t *testing.T) {
	f := newPlanFixture(t)
	step := f.action("step", "current-step", stepInput("Inspect fixture", []string{"origin"}, "goal", 0)).ID
	e := claimedPlanExecution(f, step)
	f.tx(func(tx *Tx) error {
		_, err := tx.transactionQueries.Exec("UPDATE pwnmesh_project_orchestration SET version=0 WHERE project_id='proj_001'")
		return err
	})
	before, _ := json.Marshal(f.state())
	f.tx(func(tx *Tx) error {
		requireAPIStatus(t, tx.RegisterExecution(e), 409)
		_, err := tx.StateAction(e.ProjectID, f.fence, StateAction{Op: "step", IdempotencyKey: "retired-step", Payload: json.RawMessage(`{"action":"add","description":"Retired write","from":["origin"]}`)})
		requireAPIStatus(t, err, 409)
		_, err = tx.ConcludeEvidenceStep(e.ProjectID, e.Fence(), "f001", nil)
		requireAPIStatus(t, err, 409)
		return nil
	})
	after, _ := json.Marshal(f.state())
	if !bytes.Equal(before, after) {
		t.Fatal("retired mutation changed retained state")
	}
}
