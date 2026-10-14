//go:build linux

package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
	"pwnmesh/internal/agent"
)

// committedTranscript is an identity-validated read boundary. Consumers can
// inspect the saved request view and the committed original message stream,
// without reading session fields or widening the durable journal checkpoint.
type committedTranscript struct {
	history    []agent.Message
	path       string
	checkpoint journalCheckpoint
}

func openCommittedTranscript(runDir string, identity executionIdentity, raw []byte, result Result) (committedTranscript, error) {
	var saved session
	if err := json.Unmarshal(raw, &saved); err != nil {
		return committedTranscript{}, errors.New("published fact requires a readable session")
	}
	if err := saved.validate(identity); err != nil {
		return committedTranscript{}, err
	}
	if saved.Result == nil || saved.Result.Status != "success" || saved.Result.Text != result.Text {
		return committedTranscript{}, errors.New("published fact is not bound to the saved successful result")
	}
	return committedTranscript{history: saved.History, path: filepath.Join(runDir, "events.jsonl"), checkpoint: saved.Log}, nil
}

func (t committedTranscript) visitCurrent(consume func(agent.Message) error) error {
	for _, message := range t.history {
		if err := consume(message); err != nil {
			return err
		}
	}
	return nil
}

func (t committedTranscript) visitOriginal(ctx context.Context, consume func(agent.Message) error) error {
	return visitCommittedJournal(ctx, t.path, t.checkpoint, consume)
}

// Read only the committed prefix; a later append or uncommitted crash tail is
// not evidence for this successful session. Memory is bounded per event, not
// by loading the run's entire raw history or scanning unrelated run files.
func visitCommittedJournal(ctx context.Context, path string, checkpoint journalCheckpoint, consume func(agent.Message) error) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || checkpoint.Offset <= 0 || checkpoint.Offset > info.Size() || len(checkpoint.SHA256) != 64 {
		return errors.New("invalid committed journal boundary for published fact")
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], checkpoint.Offset-1); err != nil || last[0] != '\n' {
		return errors.New("published fact journal checkpoint is not a complete record")
	}
	digest := sha256.New()
	prefix := &io.LimitedReader{R: file, N: checkpoint.Offset}
	scanner := bufio.NewScanner(io.TeeReader(prefix, digest))
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var event agent.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		if event.Type == "message_end" && event.Message != nil {
			if err := consume(*event.Message); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if prefix.N != 0 || hex.EncodeToString(digest.Sum(nil)) != checkpoint.SHA256 {
		return errors.New("committed published fact journal SHA-256 mismatch")
	}
	return ctx.Err()
}
