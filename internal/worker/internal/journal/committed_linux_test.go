//go:build linux

package journal

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"pwnmesh/internal/agent"
)

func TestCommittedFileKeepsOffsetsAndAllowsOnlyAppendChanges(t *testing.T) {
	for _, change := range []string{"append", "prefix"} {
		t.Run(change, func(t *testing.T) {
			first := "{\"type\":\"model_call_start\"}\r\n"
			raw := []byte(first + "{\"type\":\"model_call_end\"}\n")
			path := filepath.Join(t.TempDir(), "events.jsonl")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.Seek(1, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			var offsets []int64
			err = VisitCommittedFile(context.Background(), file, bytesCheckpoint(raw), func(offset int64, event agent.Event) error {
				offsets = append(offsets, offset)
				if offset != 0 {
					return nil
				}
				if change == "prefix" {
					if _, err := file.WriteAt([]byte(" "), 0); err != nil {
						return err
					}
				}
				_, err := file.WriteAt([]byte("{\"type\":\"new_tail\"}\n"), int64(len(raw)))
				return err
			})
			if (err != nil) != (change == "prefix") {
				t.Fatalf("change %s: %v", change, err)
			}
			if !reflect.DeepEqual(offsets, []int64{0, int64(len(first))}) {
				t.Fatalf("offsets changed or tail was visited: %v", offsets)
			}
			if position, err := file.Seek(0, io.SeekCurrent); err != nil || position != 1 {
				t.Fatalf("reader moved anchored descriptor: %d %v", position, err)
			}
		})
	}
}

func TestCommittedEventsRejectInvalidRecordsAndCancellation(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("{\"type\":\"message_end\",\"text\":\"\xff\"}\n"),
		[]byte("{\"text\":\"missing type\"}\n"),
		[]byte("{\"type\":\"message_end\"}"),
	} {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := VisitCommittedEvents(context.Background(), path, bytesCheckpoint(raw), func(agent.Event) error { return nil }); err == nil {
			t.Fatalf("invalid record accepted: %q", raw)
		}
	}
	path := filepath.Join(t.TempDir(), "events.jsonl")
	raw := []byte("{\"type\":\"first\"}\n{\"type\":\"second\"}\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := VisitCommittedEvents(ctx, path, bytesCheckpoint(raw), func(agent.Event) error { calls++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("journal scan ignored cancellation: calls=%d err=%v", calls, err)
	}
}
