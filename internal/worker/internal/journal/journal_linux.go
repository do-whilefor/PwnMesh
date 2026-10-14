//go:build linux

// Package journal owns append-only Agent events and their durable byte boundary.
// Session meaning, execution policy and metrics remain with the caller.
package journal

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"pwnmesh/internal/agent"
)

// Checkpoint binds the exact committed prefix, including original JSON bytes.
type Checkpoint struct {
	Offset int64  `json:"offset"`
	SHA256 string `json:"sha256"`
}

// Recovery describes retained records when the journal was opened. These values
// cannot widen the caller's saved transcript or reset its execution allowances.
type Recovery struct {
	LastSequence   uint64
	LastCompaction uint64
	Uncommitted    int64
	PartialArchive string
}

// Log owns one run's event file. Only this owner can publish a synced checkpoint.
type Log struct {
	file           *os.File
	digest         hash.Hash
	offset         int64
	lastSequence   uint64
	lastCompaction uint64
	uncommitted    int64
	partialArchive string
}

// Complete raw records are append-only. Only an incomplete, uncommitted final
// record may be removed, after its exact bytes have been synced to an archive.
// replay observes all complete retained records, including an uncommitted tail;
// callers must discard those observations if Open returns an error.
func Open(runDir string, saved *Checkpoint, replay func(agent.Event)) (*Log, error) {
	f, err := os.OpenFile(filepath.Join(runDir, "events.jsonl"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	j := &Log{file: f, digest: sha256.New()}
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if saved == nil && stat.Size() > 0 {
		return nil, errors.New("event history exists without a bound session")
	}
	if saved != nil && (saved.Offset < 0 || saved.Offset > stat.Size()) {
		return nil, errors.New("event log is shorter than its committed checkpoint")
	}
	verified := saved == nil
	verify := func() error {
		if saved != nil && j.offset == saved.Offset {
			if hex.EncodeToString(j.digest.Sum(nil)) != saved.SHA256 {
				return errors.New("committed event log checksum mismatch")
			}
			verified = true
		}
		return nil
	}
	if err = verify(); err != nil {
		return nil, err
	}
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		if len(line) == 0 {
			break
		}
		if readErr == io.EOF {
			if !verified {
				return nil, errors.New("committed event checkpoint includes a partial record")
			}
			archive, err := os.CreateTemp(runDir, "events-incomplete-*.jsonl")
			if err != nil {
				return nil, err
			}
			if _, err = archive.Write(line); err == nil {
				err = archive.Sync()
			}
			err = errors.Join(err, archive.Close())
			if err != nil {
				return nil, err
			}
			if err = syncDirectory(runDir); err != nil {
				return nil, err
			}
			if err = f.Truncate(j.offset); err != nil {
				return nil, err
			}
			if err = f.Sync(); err != nil {
				return nil, err
			}
			j.partialArchive = filepath.Base(archive.Name())
			break
		}
		var event agent.Event
		if err = json.Unmarshal(line, &event); err != nil || event.Type == "" {
			return nil, fmt.Errorf("invalid complete event record at byte %d", j.offset)
		}
		// Historical shadow transcripts used their own sequence space.
		if !strings.HasPrefix(event.Type, "replan_") && event.Message != nil && event.Message.Sequence > j.lastSequence {
			j.lastSequence = event.Message.Sequence
		}
		if !strings.HasPrefix(event.Type, "replan_") && event.Compaction != nil && event.Compaction.ID > j.lastCompaction {
			j.lastCompaction = event.Compaction.ID
		}
		if replay != nil {
			replay(event)
		}
		j.digest.Write(line)
		j.offset += int64(len(line))
		if err = verify(); err != nil {
			return nil, err
		}
	}
	if !verified {
		return nil, errors.New("event checkpoint is not at a record boundary")
	}
	if saved != nil {
		j.uncommitted = j.offset - saved.Offset
	}
	ok = true
	return j, nil
}

func (j *Log) checkpoint() Checkpoint {
	return Checkpoint{Offset: j.offset, SHA256: hex.EncodeToString(j.digest.Sum(nil))}
}

// Append retains the existing JSON representation of an event or terminal result.
// It does not commit a session; SyncCheckpoint must precede that separate write.
func (j *Log) Append(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	n, err := j.file.Write(raw)
	if err != nil {
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	j.digest.Write(raw)
	j.offset += int64(n)
	return nil
}

// SyncCheckpoint returns a boundary only after the journal bytes are durable.
// The caller must commit its session after this succeeds, before side effects.
func (j *Log) SyncCheckpoint() (Checkpoint, error) {
	if err := j.file.Sync(); err != nil {
		return Checkpoint{}, err
	}
	return j.checkpoint(), nil
}

func (j *Log) Close() error { return j.file.Close() }

func (j *Log) Recovery() Recovery {
	return Recovery{LastSequence: j.lastSequence, LastCompaction: j.lastCompaction,
		Uncommitted: j.uncommitted, PartialArchive: j.partialArchive}
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
