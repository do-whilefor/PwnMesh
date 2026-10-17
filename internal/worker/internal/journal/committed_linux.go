//go:build linux

package journal

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"pwnmesh/internal/agent"
)

// VisitCommitted reads only the committed prefix; an append or crash tail is
// not evidence for this successful session. Memory is bounded per event, not
// by loading the run's entire raw history or scanning unrelated run files.
// Consumers collect candidate messages and must discard them if verification
// returns an error; the final checksum is known only after the prefix is read.
func VisitCommitted(ctx context.Context, path string, checkpoint Checkpoint, consume func(agent.Message) error) error {
	return VisitCommittedEvents(ctx, path, checkpoint, func(event agent.Event) error {
		if event.Type == "message_end" && event.Message != nil {
			return consume(*event.Message)
		}
		return nil
	})
}

// VisitCommittedEvents includes request observations as well as messages.
func VisitCommittedEvents(ctx context.Context, path string, checkpoint Checkpoint, consume func(agent.Event) error) error {
	return visitEvents(ctx, path, checkpoint, false, consume)
}

// VisitRecordedEvents includes retained crash records inside the saved byte
// boundary for usage accounting. Those records are not transcript evidence.
func VisitRecordedEvents(ctx context.Context, path string, checkpoint Checkpoint, consume func(agent.Event) error) error {
	return visitEvents(ctx, path, checkpoint, true, consume)
}

func visitEvents(ctx context.Context, path string, checkpoint Checkpoint, includeExcluded bool, consume func(agent.Event) error) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	return visitFile(ctx, file, checkpoint, includeExcluded, func(_ int64, event agent.Event) error { return consume(event) })
}

// VisitCommittedFile reads an already anchored descriptor without changing its
// position. Offsets identify original records, including CRLF bytes. Consumers
// must discard observations on any error, including final checksum validation.
func VisitCommittedFile(ctx context.Context, file *os.File, checkpoint Checkpoint, consume func(int64, agent.Event) error) error {
	return visitFile(ctx, file, checkpoint, false, consume)
}

func visitFile(ctx context.Context, file *os.File, checkpoint Checkpoint, includeExcluded bool, consume func(int64, agent.Event) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || checkpoint.Offset < 0 || checkpoint.Offset > info.Size() || len(checkpoint.SHA256) != 64 {
		return errors.New("invalid committed journal boundary for published fact")
	}
	if err := checkpoint.validateExclusions(); err != nil {
		return err
	}
	var last [1]byte
	if checkpoint.Offset > 0 {
		if _, err := file.ReadAt(last[:], checkpoint.Offset-1); err != nil || last[0] != '\n' {
			return errors.New("published fact journal checkpoint is not a complete record")
		}
	}
	digest := sha256.New()
	prefix := &io.LimitedReader{R: io.NewSectionReader(file, 0, checkpoint.Offset), N: checkpoint.Offset}
	scanner := bufio.NewScanner(io.TeeReader(contextReader{ctx, prefix}, digest))
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	scanner.Split(func(raw []byte, atEOF bool) (int, []byte, error) {
		if end := bytes.IndexByte(raw, '\n'); end >= 0 {
			return end + 1, raw[:end], nil
		}
		if atEOF && len(raw) > 0 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return 0, nil, nil
	})
	var offset int64
	exclusion := 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var event agent.Event
		if !utf8.Valid(scanner.Bytes()) {
			return errors.New("invalid journal UTF-8")
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		if event.Type == "" {
			return errors.New("journal record is missing its type")
		}
		end := offset + int64(len(scanner.Bytes())+1)
		excluded, err := checkpoint.excludedRecord(offset, end, &exclusion)
		if err != nil {
			return err
		}
		if !excluded || includeExcluded {
			if err := consume(offset, event); err != nil {
				return err
			}
		}
		offset = end
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if prefix.N != 0 || hex.EncodeToString(digest.Sum(nil)) != checkpoint.SHA256 {
		return errors.New("committed published fact journal SHA-256 mismatch")
	}
	after, err := file.Stat()
	if err != nil {
		return err
	}
	if after.Size() < checkpoint.Offset {
		return errors.New("committed journal was truncated while reading")
	}
	if info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		// Another request may append after the checkpoint while it is read.
		// Recheck only the bound bytes; newer observations are not this view.
		digest.Reset()
		if _, err := io.Copy(digest, contextReader{ctx, io.NewSectionReader(file, 0, checkpoint.Offset)}); err != nil {
			return err
		}
		if hex.EncodeToString(digest.Sum(nil)) != checkpoint.SHA256 {
			return errors.New("committed journal changed while reading")
		}
	}
	return ctx.Err()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(raw []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(raw[:min(len(raw), 64<<10)])
}
