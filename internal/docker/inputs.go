package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

func (c *Client) SetInputReader(fn func(context.Context, worker.Job, board.InputFile) (io.ReadCloser, error)) {
	c.graphMu.Lock()
	defer c.graphMu.Unlock()
	c.inputReader = fn
}

// Files are immutable project inputs. Serialize their installation across
// concurrent runs; hash existing bytes instead of downloading large APKs again.
func (c *Client) stageInputs(ctx context.Context, name string, j worker.Job) error {
	if len(j.InputFiles) == 0 {
		return nil
	}
	if len(j.InputFiles) > board.MaxProjectInputs {
		return errors.New("too many input files")
	}
	unlock := c.lock("inputs:" + j.Graph.Project.ID)
	defer unlock()
	c.graphMu.RLock()
	read := c.inputReader
	c.graphMu.RUnlock()
	if read == nil {
		return errors.New("input file reader is not configured")
	}
	var total int64
	for _, f := range j.InputFiles {
		if !f.Valid() {
			return errors.New("invalid input file metadata")
		}
		total += f.Size
		if total > board.MaxProjectInputBytes {
			return errors.New("project input byte limit exceeded")
		}
	}
	for _, f := range j.InputFiles {
		var output bytes.Buffer
		_, err := c.exec(ctx, name, []string{"/bin/sh", "-c", `test -f "$1" && test ! -L "$1" && exec sha256sum -- "$1"`, "input-check", f.Path}, nil, &output)
		if err == nil && strings.HasPrefix(output.String(), f.SHA256+" ") {
			continue
		}
		data, err := read(ctx, j, f)
		if err != nil {
			return err
		}
		err = c.archiveInput(ctx, name, f, data)
		closeErr := data.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func (c *Client) archiveInput(ctx context.Context, name string, f board.InputFile, source io.Reader) error {
	if !f.Valid() {
		return errors.New("invalid input file metadata")
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		tw := tar.NewWriter(writer)
		err := writeInputTar(tw, f, source)
		if err == nil {
			err = tw.Close()
		}
		_ = writer.CloseWithError(err)
		done <- err
	}()
	res, err := c.request(ctx, "PUT", "/containers/"+url.PathEscape(name)+"/archive?path=%2Fworkspace&noOverwriteDirNonDir=1", reader, "application/x-tar")
	_ = reader.CloseWithError(err)
	writeErr := <-done
	if res != nil {
		res.Body.Close()
	}
	return errors.Join(err, writeErr)
}

func writeInputTar(tw *tar.Writer, f board.InputFile, source io.Reader) error {
	for _, dir := range []string{".pwnmesh", ".pwnmesh/inputs", ".pwnmesh/inputs/" + f.ID} {
		if err := tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0700}); err != nil {
			return err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(f.Path, "/workspace/"), Mode: 0444, Size: f.Size}); err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(tw, h), source, f.Size); err != nil {
		return err
	}
	var extra [1]byte
	if n, err := io.ReadFull(source, extra[:]); n != 0 || err != io.EOF {
		return fmt.Errorf("input file size mismatch")
	}
	if hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return errors.New("input file SHA-256 mismatch")
	}
	return nil
}
