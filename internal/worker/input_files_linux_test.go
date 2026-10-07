//go:build linux

package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

func TestWorkerVerifiesInputBytesBeforeModel(t *testing.T) {
	root := t.TempDir()
	data := []byte{0, 255, 'P', 'K'}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	f := board.InputFile{ID: digest, Name: "app.apk", SHA256: digest, Size: int64(len(data)), Path: "/workspace/.pwnmesh/inputs/" + digest + "/app.apk"}
	file := filepath.Join(root, ".pwnmesh", "inputs", digest, f.Name)
	_ = os.MkdirAll(filepath.Dir(file), 0700)
	_ = os.WriteFile(file, data, 0600)
	j := Job{Workspace: root, InputFiles: []board.InputFile{f}}
	if err := verifyInputFiles(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(file, []byte("BAD!"), 0600)
	if verifyInputFiles(context.Background(), j) == nil {
		t.Fatal("changed input accepted")
	}
	_ = os.Remove(file)
	other := filepath.Join(root, "other")
	_ = os.WriteFile(other, data, 0600)
	_ = os.Symlink(other, file)
	if verifyInputFiles(context.Background(), j) == nil {
		t.Fatal("symlink accepted")
	}
	j.InputFiles[0].Path = "/workspace/../../escape"
	if verifyInputFiles(context.Background(), j) == nil {
		t.Fatal("invalid path accepted")
	}
}

func TestCachedWorkerGraphStillVerifiesUploadedInputs(t *testing.T) {
	j, dir := graphWrapperJob(t), t.TempDir()
	data := []byte("immutable input")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	input := board.InputFile{ID: digest, Name: "config.json", SHA256: digest, Size: int64(len(data)), Path: "/workspace/.pwnmesh/inputs/" + digest + "/config.json"}
	file := filepath.Join(j.Workspace, ".pwnmesh", "inputs", digest, input.Name)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	j.InputFiles = []board.InputFile{input}
	var output bytes.Buffer
	calls := 0
	opts := Options{RunDir: dir, Output: &output, Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		calls++
		return agent.Text("assistant", completedOutput("explore")), nil
	})}
	for n := 0; n < 2; n++ {
		r, err := runTestWorker(context.Background(), j, opts)
		if err != nil || r.Status != "success" {
			t.Fatalf("initial/cache run: %+v %v", r, err)
		}
	}
	if calls != 1 {
		t.Fatalf("graph did not reuse result: %d", calls)
	}
	if err := os.WriteFile(file, bytes.Repeat([]byte{'x'}, len(data)), 0600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	r, err := runTestWorker(context.Background(), j, opts)
	if err == nil || r.Status == "success" || calls != 1 || len(graphOutputResults(t, output.Bytes())) != 0 {
		t.Fatalf("cached result bypassed input verification: %+v %v", r, err)
	}
}
