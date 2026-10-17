//go:build linux

package journal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"pwnmesh/internal/agent"
)

func bytesCheckpoint(raw []byte) Checkpoint {
	digest := sha256.Sum256(raw)
	return Checkpoint{Offset: int64(len(raw)), SHA256: hex.EncodeToString(digest[:])}
}

func TestRecoveryPreservesRawPrefixAndArchivesOnlyPartialTail(t *testing.T) {
	dir := t.TempDir()
	// Historical spacing and field order are part of the durable hash. Neither
	// replay nor a later checkpoint may replace them with re-encoded JSON.
	committed := []byte("{\"message\":{\"sequence\":2,\"role\":\"assistant\",\"content\":[]}, \"type\":\"message_end\"}\n" +
		"{\"type\":\"replan_message_end\",\"message\":{\"sequence\":999,\"role\":\"assistant\"}}\n" +
		"{\"type\":\"replan_compaction\",\"compaction\":{\"id\":999}}\n" +
		"{\"type\":\"unknown\",\"message\":{\"sequence\":1000},\"compaction\":{\"id\":1000}}\n")
	tail := []byte("{\"type\":\"message_end\",\"message\":{\"sequence\":4,\"role\":\"assistant\"}}\n" +
		"{\"type\":\"context_compacted\",\"compaction\":{\"id\":3}}\n")
	partial := []byte("{\"type\":\"message_end\",\"message\":")
	complete := append(append([]byte{}, committed...), tail...)
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), append(append([]byte{}, complete...), partial...), 0600); err != nil {
		t.Fatal(err)
	}
	saved := bytesCheckpoint(committed)
	var replayed []string
	log, err := Open(dir, &saved, func(event agent.Event) { replayed = append(replayed, event.Type) })
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	recovery := log.Recovery()
	if recovery.LastSequence != 4 || recovery.LastCompaction != 3 || recovery.Uncommitted != int64(len(tail)) || recovery.PartialArchive == "" {
		t.Fatalf("recovery mixed cursor spaces or lost the tail: %+v", recovery)
	}
	wantTypes := []string{"message_end", "replan_message_end", "replan_compaction", "unknown", "message_end", "context_compacted"}
	if !reflect.DeepEqual(replayed, wantTypes) {
		t.Fatalf("replay changed original records: got %v, want %v", replayed, wantTypes)
	}
	archive, err := os.ReadFile(filepath.Join(dir, recovery.PartialArchive))
	if err != nil || !bytes.Equal(archive, partial) {
		t.Fatalf("partial archive changed: %q, %v", archive, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil || !bytes.Equal(raw, complete) {
		t.Fatalf("complete records were rewritten or removed: %q, %v", raw, err)
	}
	checkpoint, err := log.SyncCheckpoint()
	want := bytesCheckpoint(complete)
	want.Excluded = []ByteRange{{Start: int64(len(committed)), End: int64(len(complete))}}
	if err != nil || !reflect.DeepEqual(checkpoint, want) {
		t.Fatalf("checkpoint lost exact retained bytes: %+v, %v", checkpoint, err)
	}
	var sequences []uint64
	if err := VisitCommitted(context.Background(), filepath.Join(dir, "events.jsonl"), saved, func(message agent.Message) error {
		sequences = append(sequences, message.Sequence)
		return nil
	}); err != nil || !reflect.DeepEqual(sequences, []uint64{2}) {
		t.Fatalf("committed evidence admitted a shadow or crash tail: %v, %v", sequences, err)
	}
	// Appending after repair must continue the original hash stream.
	event := agent.Event{Type: "recovery", Text: "retained original records"}
	if err := log.Append(event); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(event)
	complete = append(complete, append(encoded, '\n')...)
	checkpoint, err = log.SyncCheckpoint()
	want.Offset, want.SHA256 = bytesCheckpoint(complete).Offset, bytesCheckpoint(complete).SHA256
	if err != nil || !reflect.DeepEqual(checkpoint, want) {
		t.Fatalf("append diverged from exact-byte hash: %+v, %v", checkpoint, err)
	}
}

func TestClosedJournalCannotPublishCheckpoint(t *testing.T) {
	log, err := Open(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(agent.Event{Type: "message_end"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if checkpoint, err := log.SyncCheckpoint(); err == nil || !reflect.DeepEqual(checkpoint, Checkpoint{}) {
		t.Fatalf("closed journal published a boundary: %+v, %v", checkpoint, err)
	}
	if err := log.Append(agent.Event{Type: "recovery"}); err == nil {
		t.Fatal("closed journal accepted another event")
	}
}

func TestRecoveryCursorUsesCurrentEventKind(t *testing.T) {
	for _, kind := range []string{"message_end", "context_compaction_failed", "context_compaction_prepared", "context_compacted", "replan_message_end", "replan_compaction", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			event := agent.Event{Type: kind, Message: &agent.Message{Sequence: 7}, Compaction: &agent.CompactionRecord{CompactionCheckpoint: agent.CompactionCheckpoint{ID: 9}}}
			raw, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, '\n')
			if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			checkpoint := bytesCheckpoint(raw)
			log, err := Open(dir, &checkpoint, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			var sequence, compaction uint64
			switch kind {
			case "message_end":
				sequence = 7
			case "context_compaction_failed", "context_compaction_prepared", "context_compacted":
				compaction = 9
			}
			if got := log.Recovery(); got.LastSequence != sequence || got.LastCompaction != compaction {
				t.Fatalf("event payload crossed recovery cursor boundaries: %+v", got)
			}
		})
	}
}

func TestRecoveryKeepsCrashTailExcludedAcrossCheckpoints(t *testing.T) {
	dir := t.TempDir()
	log, err := Open(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	appendMessage := func(log *Log, sequence uint64, text string) {
		message := agent.Text("assistant", text)
		message.Sequence = sequence
		if err := log.Append(agent.Event{Type: "message_end", Message: &message}); err != nil {
			t.Fatal(err)
		}
	}
	appendMessage(log, 1, "committed")
	saved, err := log.SyncCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	appendMessage(log, 2, "crash tail")
	if err := log.Append(agent.Event{Type: "model_call_end", Text: "reported usage survives"}); err != nil {
		t.Fatal(err)
	}
	log.Close()
	reported := 0
	log, err = Open(dir, &saved, func(event agent.Event) {
		if event.Type == "model_call_end" {
			reported++
		}
	})
	if err != nil || reported != 1 {
		t.Fatalf("recovery lost billed observations: %d %v", reported, err)
	}
	appendMessage(log, 3, "after recovery")
	saved, err = log.SyncCheckpoint()
	if err != nil || len(saved.Excluded) != 1 {
		t.Fatalf("recovery lost exclusion: %+v %v", saved, err)
	}
	log.Close()
	// Session serialization and repeated recovery must preserve the same
	// original exclusion, even after newer messages become committed.
	raw, _ := json.Marshal(saved)
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	log, err = Open(dir, &saved, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	again, err := log.SyncCheckpoint()
	if err != nil || !reflect.DeepEqual(again, saved) {
		t.Fatalf("later recovery changed exclusion: %+v %v", again, err)
	}
	var messages []string
	if err := VisitCommitted(context.Background(), filepath.Join(dir, "events.jsonl"), again, func(message agent.Message) error {
		messages = append(messages, message.Text())
		return nil
	}); err != nil || !reflect.DeepEqual(messages, []string{"committed", "after recovery"}) {
		t.Fatalf("later save promoted crash-tail evidence: %v %v", messages, err)
	}
	file, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte(" "), saved.Excluded[0].Start); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := VisitCommitted(context.Background(), filepath.Join(dir, "events.jsonl"), again, func(agent.Message) error { return nil }); err == nil {
		t.Fatal("excluded bytes escaped full-prefix verification")
	}
}
