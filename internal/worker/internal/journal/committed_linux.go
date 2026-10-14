//go:build linux

package journal

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
	"pwnmesh/internal/agent"
)

// VisitCommitted reads only the committed prefix; an append or crash tail is
// not evidence for this successful session. Memory is bounded per event, not
// by loading the run's entire raw history or scanning unrelated run files.
// Consumers collect candidate messages and must discard them if verification
// returns an error; the final checksum is known only after the prefix is read.
func VisitCommitted(ctx context.Context, path string, checkpoint Checkpoint, consume func(agent.Message) error) error {
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
