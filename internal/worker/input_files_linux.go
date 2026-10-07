package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Check metadata and original bytes before starting the model, including on
// recovery. Shared workspaces coordinate writers; they are not a sandbox.
func verifyInputFiles(ctx context.Context, j Job) error {
	for _, input := range j.InputFiles {
		if !input.Valid() {
			return errors.New("invalid input file metadata")
		}
		rel := strings.TrimPrefix(input.Path, "/workspace/")
		file := filepath.Join(j.Workspace, rel)
		for p := file; p != filepath.Clean(j.Workspace); p = filepath.Dir(p) {
			info, err := os.Lstat(p)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return errors.New("input path contains a symlink")
			}
			if p == file && (!info.Mode().IsRegular() || info.Size() != input.Size) {
				return errors.New("input file size or type mismatch")
			}
		}
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, &inputContextReader{ctx: ctx, r: f})
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if hex.EncodeToString(h.Sum(nil)) != input.SHA256 {
			return fmt.Errorf("input file %s SHA-256 mismatch", input.ID)
		}
	}
	return nil
}

type inputContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *inputContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
