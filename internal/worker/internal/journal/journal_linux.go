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
	"slices"

	"pwnmesh/internal/agent"
)

// Checkpoint binds the exact committed prefix, including original JSON bytes.
type Checkpoint struct {
	Offset   int64       `json:"offset"`
	SHA256   string      `json:"sha256"`
	Excluded []ByteRange `json:"excluded,omitempty"`
}

// ByteRange retains a complete crash tail for audit and usage replay without
// promoting it to committed transcript evidence when later records are saved.
type ByteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

func (c Checkpoint) validateExclusions() error {
	var end int64
	for _, span := range c.Excluded {
		if span.Start < end || span.End <= span.Start || span.End > c.Offset {
			return errors.New("invalid excluded journal range")
		}
		end = span.End
	}
	return nil
}

func (c Checkpoint) excludedRecord(start, end int64, cursor *int) (bool, error) {
	for *cursor < len(c.Excluded) && c.Excluded[*cursor].End <= start {
		*cursor++
	}
	if *cursor < len(c.Excluded) {
		span := c.Excluded[*cursor]
		if span.Start < end {
			if span.Start > start || span.End < end {
				return false, errors.New("excluded journal range is not at a record boundary")
			}
			return true, nil
		}
	}
	return false, nil
}

// Prefix keeps server-owned exclusions when binding an older read checkpoint.
func (c Checkpoint) Prefix(offset int64, sha256 string) (Checkpoint, error) {
	if err := c.validateExclusions(); err != nil {
		return Checkpoint{}, err
	}
	if offset < 0 || offset > c.Offset {
		return Checkpoint{}, errors.New("invalid journal prefix offset")
	}
	bound := Checkpoint{Offset: offset, SHA256: sha256}
	for _, span := range c.Excluded {
		if span.Start >= offset {
			break
		}
		bound.Excluded = append(bound.Excluded, ByteRange{Start: span.Start, End: min(span.End, offset)})
	}
	return bound, nil
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
	excluded       []ByteRange
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
	if saved != nil {
		if err := saved.validateExclusions(); err != nil {
			return nil, err
		}
		j.excluded = slices.Clone(saved.Excluded)
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
	exclusion := 0
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
		if saved != nil {
			if _, err := saved.excludedRecord(j.offset, j.offset+int64(len(line)), &exclusion); err != nil {
				return nil, err
			}
		}
		// Only current transcript events advance recovery cursors. Other
		// complete records remain in the raw journal without interpretation.
		switch event.Type {
		case "message_end":
			if event.Message != nil && event.Message.Sequence > j.lastSequence {
				j.lastSequence = event.Message.Sequence
			}
		case "context_compaction_failed", "context_compaction_prepared", "context_compacted":
			if event.Compaction != nil && event.Compaction.ID > j.lastCompaction {
				j.lastCompaction = event.Compaction.ID
			}
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
		if j.uncommitted > 0 {
			j.excluded = append(j.excluded, ByteRange{Start: saved.Offset, End: j.offset})
		}
	}
	ok = true
	return j, nil
}

func (j *Log) checkpoint() Checkpoint {
	return Checkpoint{Offset: j.offset, SHA256: hex.EncodeToString(j.digest.Sum(nil)), Excluded: slices.Clone(j.excluded)}
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
