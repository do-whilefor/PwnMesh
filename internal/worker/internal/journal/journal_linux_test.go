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
		"{\"type\":\"replan_compaction\",\"compaction\":{\"id\":999}}\n")
	tail := []byte("{\"type\":\"message_end\",\"message\":{\"sequence\":4,\"role\":\"assistant\"}}\n" +
		"{\"type\":\"context_compaction\",\"compaction\":{\"id\":3}}\n")
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
	wantTypes := []string{"message_end", "replan_message_end", "replan_compaction", "message_end", "context_compaction"}
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
	if err != nil || checkpoint != bytesCheckpoint(complete) {
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
	if err != nil || checkpoint != bytesCheckpoint(complete) {
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
	if checkpoint, err := log.SyncCheckpoint(); err == nil || checkpoint != (Checkpoint{}) {
		t.Fatalf("closed journal published a boundary: %+v, %v", checkpoint, err)
	}
	if err := log.Append(agent.Event{Type: "recovery"}); err == nil {
		t.Fatal("closed journal accepted another event")
	}
}
