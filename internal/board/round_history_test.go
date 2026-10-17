package board

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func roundFixture(t *testing.T) *orchestrationFixture {
	t.Helper()
	f := newOrchestrationFixture(t)
	e := f.worker("evidence-one", "")
	first := f.fact(e, "first")
	evidence := make([]EvidenceRef, 8)
	for n := range evidence {
		evidence[n] = EvidenceRef{RunID: e.Lease, Path: fmt.Sprintf("/workspace/evidence-%d.txt", n), Excerpt: strings.Repeat("evidence🙂", 600)}
	}
	f.action(e.Fence(), "fact", "large-evidence", map[string]any{"description": "Retain every evidence byte", "scope": "fixture", "observed_at": f.store.Now().Format("2006-01-02T15:04:05Z"), "evidence": evidence}, "")
	candidate := f.candidate(e, first, "verified")
	curator, state := f.curator("archive-curation")
	f.curate(curator, state, CurateGroup{CandidateIDs: []string{candidate}, Status: "verified", Reason: "Retain reviewed evidence"})
	return f
}

func TestRestartArchivesCompleteRoundAndReadsEveryByte(t *testing.T) {
	f := roundFixture(t)
	before := f.state()
	expected := map[string][]byte{}
	expected["state/state"], _ = json.Marshal(before)
	expected["inputs/inputs"] = []byte("[]")
	f.do(func(tx *Tx) error {
		events, err := tx.StateEvents("p", 0)
		if err != nil {
			return err
		}
		for _, event := range events {
			expected[fmt.Sprintf("event/%d", event.Revision)], _ = json.Marshal(event)
		}
		rows, err := tx.transactionQueries.Query("SELECT idempotency_key,request,response FROM pwnmesh_state_actions WHERE project_id=?", "p")
		if err != nil {
			return err
		}
		for rows.Next() {
			var key, request, response string
			if err = rows.Scan(&key, &request, &response); err != nil {
				rows.Close()
				return err
			}
			expected["action/"+key], _ = json.Marshal(map[string]string{"request": request, "response": response})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range []string{"evidence-one", "archive-curation"} {
			execution, err := tx.Execution("p", id)
			if err != nil {
				return err
			}
			expected["execution/"+execution.ID], _ = json.Marshal(execution)
		}
		return nil
	})
	if len(expected["state/state"]) < 32<<10 {
		t.Fatal("fixture does not exercise byte continuation")
	}
	zero := int64(0)
	f.do(func(tx *Tx) error { _, err := tx.RestartProject("p", &zero); return err })
	current := f.state()
	if current.Graph.Project.Generation != 1 || len(current.Graph.Facts) != 2 || len(current.Steps) != 0 || len(current.Findings) != 0 || current.Revision != 0 {
		t.Fatalf("restart did not begin a clean projection: %+v", current)
	}
	seen, cursor, pages := map[string]bool{}, int64(0), 0
	for {
		var page RoundEntryPage
		f.do(func(tx *Tx) (err error) { page, err = tx.RoundEntries("p", 0, cursor, 2); return err })
		pages++
		if len(page.Items) > 2 {
			t.Fatal("entry pagination exceeded limit")
		}
		for _, entry := range page.Items {
			key := entry.Kind + "/" + entry.ID
			want, ok := expected[key]
			if !ok || seen[key] || entry.Cursor <= cursor {
				t.Fatalf("unexpected or duplicate archive entry: %+v", entry)
			}
			seen[key] = true
			var restored []byte
			offset := int64(0)
			for {
				var chunk RoundEntryChunk
				// This prime byte length intentionally splits multi-byte UTF-8.
				f.do(func(tx *Tx) (err error) {
					chunk, err = tx.ReadRoundEntry("p", 0, entry.Cursor, offset, 1009)
					return err
				})
				if chunk.Encoding != "base64" || chunk.Offset != offset || chunk.TotalBytes != entry.Bytes || chunk.SHA256 != entry.SHA256 || len(chunk.Data) > 1009 {
					t.Fatalf("inconsistent chunk metadata for %s", key)
				}
				restored = append(restored, chunk.Data...)
				if chunk.NextOffset == nil {
					break
				}
				if *chunk.NextOffset != int64(len(restored)) || *chunk.NextOffset <= offset {
					t.Fatal("archive continuation did not advance exactly")
				}
				offset = *chunk.NextOffset
			}
			sum := sha256.Sum256(restored)
			if !bytes.Equal(restored, want) || entry.Bytes != int64(len(want)) || entry.SHA256 != hex.EncodeToString(sum[:]) {
				t.Fatalf("archive changed bytes or checksum for %s", key)
			}
		}
		if page.NextCursor == nil {
			break
		}
		if *page.NextCursor <= cursor {
			t.Fatal("archive cursor did not advance")
		}
		cursor = *page.NextCursor
	}
	if len(seen) != len(expected) || pages < 2 {
		t.Fatalf("incomplete archive: read=%d expected=%d pages=%d", len(seen), len(expected), pages)
	}
	// A second restart adds a round without changing any prior archived bytes.
	one := int64(1)
	f.do(func(tx *Tx) error { _, err := tx.RestartProject("p", &one); return err })
	f.do(func(tx *Tx) error {
		var prior []byte
		if err := tx.transactionQueries.QueryRow("SELECT data FROM pwnmesh_round_entries WHERE project_id=? AND generation=0 AND kind='state' AND id='state'", "p").Scan(&prior); err != nil {
			return err
		}
		if !bytes.Equal(prior, expected["state/state"]) {
			t.Fatal("second restart changed the first archived round")
		}
		first, err := tx.RoundHistory("p", -1, 1)
		if err != nil {
			return err
		}
		if len(first.Items) != 1 || first.Items[0].Generation != 0 || first.NextCursor == nil || *first.NextCursor != 0 {
			t.Fatalf("first generation page: %+v", first)
		}
		last, err := tx.RoundHistory("p", *first.NextCursor, 1)
		if len(last.Items) != 1 || last.Items[0].Generation != 1 || last.NextCursor != nil {
			t.Fatalf("second generation page: %+v", last)
		}
		return err
	})
}

func TestRoundInputManifestUsesTheArchiveTransactionNotTimestamps(t *testing.T) {
	f := newOrchestrationFixture(t)
	var first, second InputFile
	f.do(func(tx *Tx) error {
		var err error
		first, err = tx.AddInput("p", "before.txt", []byte("original input"))
		return err
	})
	f.do(func(tx *Tx) error { _, err := tx.RestartProject("p", nil); return err })
	f.do(func(tx *Tx) error {
		var err error
		second, err = tx.AddInput("p", "after.txt", []byte("next round input"))
		return err
	})
	if first.CreatedAt != second.CreatedAt {
		t.Fatal("fixture must reproduce uploads and restart within one second")
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.do(func(tx *Tx) error {
		var raw []byte
		if err := tx.transactionQueries.QueryRow("SELECT data FROM pwnmesh_round_entries WHERE project_id='p' AND generation=0 AND kind='inputs' AND id='inputs'").Scan(&raw); err != nil {
			return err
		}
		var inputs []InputFile
		if err := json.Unmarshal(raw, &inputs); err != nil {
			return err
		}
		if len(inputs) != 1 || inputs[0] != first {
			t.Fatalf("archive substituted current inputs: %+v", inputs)
		}
		current, err := tx.InputFiles("p")
		if err == nil && len(current) != 2 {
			t.Fatal("current project lost retained inputs")
		}
		return err
	})
}

func TestRestartArchiveFailureRollsBackEntireRound(t *testing.T) {
	f := roundFixture(t)
	before, _ := json.Marshal(f.state())
	counts := map[string]int{}
	f.do(func(tx *Tx) error {
		for _, table := range []string{"pwnmesh_round_history", "pwnmesh_round_entries", "pwnmesh_revoked_runs"} {
			var count int
			if err := tx.transactionQueries.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE project_id='p'").Scan(&count); err != nil {
				return err
			}
			counts[table] = count
		}
		return nil
	})
	f.do(func(tx *Tx) error {
		_, err := tx.transactionQueries.Exec(`CREATE TRIGGER fail_round_archive BEFORE INSERT ON pwnmesh_round_entries WHEN NEW.kind='event' BEGIN SELECT RAISE(ABORT,'archive write failed'); END`)
		return err
	})
	zero := int64(0)
	err := f.store.Do(context.Background(), func(tx *Tx) error { _, err := tx.RestartProject("p", &zero); return err })
	if err == nil || !strings.Contains(err.Error(), "archive write failed") {
		t.Fatalf("archive failure was not returned: %v", err)
	}
	after, _ := json.Marshal(f.state())
	if !bytes.Equal(before, after) {
		t.Fatal("archive failure modified live state")
	}
	f.do(func(tx *Tx) error {
		for _, table := range []string{"pwnmesh_round_history", "pwnmesh_round_entries", "pwnmesh_revoked_runs"} {
			var count int
			if err := tx.transactionQueries.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE project_id=?", "p").Scan(&count); err != nil {
				return err
			}
			if count != counts[table] {
				t.Fatalf("failed restart changed %s from %d rows to %d", table, counts[table], count)
			}
		}
		if _, err := tx.Execution("p", "evidence-one"); err != nil {
			return err
		}
		_, err := tx.transactionQueries.Exec("DROP TRIGGER fail_round_archive")
		return err
	})
	f.do(func(tx *Tx) error { _, err := tx.RestartProject("p", &zero); return err })
	err = f.store.Do(context.Background(), func(tx *Tx) error { _, err := tx.RestartProject("p", &zero); return err })
	var api *APIError
	if !errors.As(err, &api) || api.Status != 409 || f.state().Graph.Project.Generation != 1 {
		t.Fatalf("replayed expected_generation restarted again: %v", err)
	}
}

func TestRoundReadsPreserveValidationMissingPolicyAndEndOffsets(t *testing.T) {
	f := newOrchestrationFixture(t)
	f.do(func(tx *Tx) error {
		page, err := tx.RoundHistory("p", -1, 100)
		if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
			t.Fatalf("empty history changed response shape: %+v, %v", page, err)
		}
		_, err = tx.RestartProject("p", nil)
		return err
	})
	f.do(func(tx *Tx) error {
		page, err := tx.RoundEntries("p", 0, 0, 100)
		if err != nil {
			return err
		}
		entry := page.Items[0]
		chunk, err := tx.ReadRoundEntry("p", 0, entry.Cursor, entry.Bytes, 1)
		if err != nil || len(chunk.Data) != 0 || chunk.TotalBytes != entry.Bytes || chunk.Offset != entry.Bytes || chunk.NextOffset != nil || chunk.SHA256 != entry.SHA256 || chunk.Encoding != "base64" {
			t.Fatalf("exact end offset changed response: %+v, %v", chunk, err)
		}
		for _, check := range []struct {
			name   string
			status int
			read   func() error
		}{
			{"history cursor", 422, func() error { _, err := tx.RoundHistory("p", -2, 1); return err }},
			{"history limit", 422, func() error { _, err := tx.RoundHistory("p", -1, 101); return err }},
			{"missing project", 404, func() error { _, err := tx.RoundHistory("missing", -1, 1); return err }},
			{"entry cursor", 422, func() error { _, err := tx.RoundEntries("p", 0, -1, 1); return err }},
			{"entry limit", 422, func() error { _, err := tx.RoundEntries("p", 0, 0, 0); return err }},
			{"missing round", 404, func() error { _, err := tx.RoundEntries("p", 1, 0, 1); return err }},
			{"byte limit", 422, func() error { _, err := tx.ReadRoundEntry("p", 0, entry.Cursor, 0, 32769); return err }},
			{"negative offset", 422, func() error { _, err := tx.ReadRoundEntry("p", 0, entry.Cursor, -1, 1); return err }},
			{"past end offset", 422, func() error { _, err := tx.ReadRoundEntry("p", 0, entry.Cursor, entry.Bytes+1, 1); return err }},
			{"other generation", 404, func() error { _, err := tx.ReadRoundEntry("p", 1, entry.Cursor, 0, 1); return err }},
			{"other project", 404, func() error { _, err := tx.ReadRoundEntry("missing", 0, entry.Cursor, 0, 1); return err }},
		} {
			var api *APIError
			if err := check.read(); !errors.As(err, &api) || api.Status != check.status {
				t.Fatalf("%s changed error policy: %v", check.name, err)
			}
		}
		return nil
	})
}
