package docker

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"pwnmesh/internal/board"
)

func inputFixture(data []byte) board.InputFile {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	name := "capture.har"
	return board.InputFile{ID: digest, Name: name, Size: int64(len(data)), SHA256: digest, Path: "/workspace/.pwnmesh/inputs/" + digest + "/" + name}
}

func TestInputArchivePreservesBytesAndRejectsCorruption(t *testing.T) {
	data := []byte{0, 255, '\r', '\n', 'P', 'K'}
	f := inputFixture(data)
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := writeInputTar(tw, f, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&archive)
	files := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		files++
		got, _ := io.ReadAll(tr)
		if !bytes.Equal(got, data) || h.Name != ".pwnmesh/inputs/"+f.ID+"/"+f.Name || h.Mode != 0444 {
			t.Fatalf("bad archive entry: %+v", h)
		}
	}
	if files != 1 {
		t.Fatal("unexpected file count")
	}
	for _, bad := range [][]byte{data[:2], append(append([]byte{}, data...), 1), bytes.Repeat([]byte{'x'}, len(data))} {
		tw := tar.NewWriter(io.Discard)
		if err := writeInputTar(tw, f, bytes.NewReader(bad)); err == nil {
			t.Fatal("corrupt input accepted")
		}
	}
}
