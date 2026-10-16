//go:build linux

package workergraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ReadCheckpoint applies the same filesystem and JSON policy to recovery and
// read-only inspection. It never creates, rewrites or executes anything.
// Callback errors and skip reasons have no writer-side size limit, so a reader
// cap would reject valid retained state. Stream the JSON without an extra full
// byte copy; a future byte limit must bound both writers and readers together.
func ReadCheckpoint(ctx context.Context, dir string) (checkpoint Checkpoint, err error) {
	if err := ctx.Err(); err != nil {
		return checkpoint, err
	}
	path, err := filepath.Abs(filepath.Join(dir, "graph.json"))
	if err != nil {
		return checkpoint, err
	}
	// Validate the directory even when graph.json does not exist yet. A fresh
	// run must not write through an alias that its next recovery would reject.
	resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return checkpoint, err
	}
	if resolvedDir != filepath.Dir(path) {
		return checkpoint, errors.New("graph checkpoint directory must not use symlinks")
	}
	// NONBLOCK prevents a malformed FIFO from waiting for a writer before we
	// can reject it. NOFOLLOW also rejects dangling checkpoint symlinks.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return checkpoint, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	defer func() {
		if err != nil {
			// Only an absent initial open authorizes a fresh graph. Once opened,
			// disappearance is a read failure, even if its cause is ENOENT.
			if errors.Is(err, os.ErrNotExist) {
				err = fmt.Errorf("graph checkpoint disappeared while reading: %v", err)
			} else {
				err = fmt.Errorf("read existing graph checkpoint: %w", err)
			}
		}
	}()
	before, err := file.Stat()
	if err != nil {
		return checkpoint, err
	}
	if !before.Mode().IsRegular() {
		return checkpoint, errors.New("graph checkpoint must be a regular file")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return checkpoint, err
	}
	if resolved != path {
		return checkpoint, errors.New("graph checkpoint must not use symlinks")
	}
	reader := checkpointReader{ctx: ctx, file: file}
	decoder := json.NewDecoder(&reader)
	if err := decoder.Decode(&checkpoint); err != nil {
		return checkpoint, err
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		if err != nil {
			return checkpoint, err
		}
		return checkpoint, errors.New("graph checkpoint requires exactly one JSON value")
	}
	after, err := file.Stat()
	if err != nil {
		return checkpoint, err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return checkpoint, err
	}
	if reader.size != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || !os.SameFile(before, current) {
		return checkpoint, errors.New("graph checkpoint changed while reading")
	}
	return checkpoint, ctx.Err()
}

type checkpointReader struct {
	ctx  context.Context
	file *os.File
	size int64
}

func (r *checkpointReader) Read(raw []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.file.Read(raw[:min(len(raw), 64<<10)])
	r.size += int64(n)
	return n, err
}
