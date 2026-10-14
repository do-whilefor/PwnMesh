//go:build linux

package dag

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"pwnmesh/internal/process"
	"pwnmesh/internal/workergraph"
)

// The run root is already identity-bound by runSession. Reject symlinked child
// directories so a reused key cannot redirect checkpoint or artifact writes.
func commandGraphDirectory(root string, elements ...string) (string, error) {
	dir := root
	for _, element := range elements {
		dir = filepath.Join(dir, element)
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return "", err
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("command graph directory must be a real directory")
		}
	}
	return dir, nil
}

func verifyCommandNodeOutput(ctx context.Context, dir string, spec Node, out workergraph.Output) error {
	var value Output
	if json.Unmarshal(out.Value, &value) != nil || value.ExitCode != 0 || value.ReusedFrom != nil || value.OutputPath != filepath.Join(dir, "stdout.log") {
		return errors.New("invalid command node output")
	}
	paths := append([]string{"stdout.log"}, spec.Artifacts...)
	if spec.Kind != "agent" {
		if value.StderrPath != filepath.Join(dir, "stderr.log") {
			return errors.New("invalid command node stderr path")
		}
		paths = append(paths, "stderr.log")
	} else if value.StderrPath != "" || value.Stderr != "" {
		return errors.New("agent node cannot contain command stderr")
	}
	if len(out.Artifacts) != len(paths) {
		return errors.New("invalid command node artifacts")
	}
	for i, path := range paths {
		previewLimit := 0
		if i == 0 || path == "stderr.log" {
			previewLimit = 8000
		}
		artifact, preview, err := commandFile(ctx, dir, path, previewLimit)
		if err != nil {
			return err
		}
		if out.Artifacts[i] != artifact {
			return errors.New("command node artifact SHA-256 changed")
		}
		if i == 0 && value.Stdout != preview {
			return errors.New("command node stdout preview differs from its artifact")
		}
		if path == "stderr.log" && value.Stderr != preview {
			return errors.New("command node stderr preview differs from its artifact")
		}
	}
	return nil
}

func executeCommandNode(ctx context.Context, runDir, workspace, dir string, spec Node, dependencies []workergraph.NodeState) (workergraph.Output, error) {
	views, err := freezeCommandInputs(ctx, dir, spec, dependencies)
	if err != nil {
		return workergraph.Output{}, err
	}
	deps, err := json.Marshal(views)
	if err != nil {
		return workergraph.Output{}, err
	}
	depsPath := filepath.Join(dir, "dependencies.json")
	if err := commandNewFile(depsPath, deps); err != nil {
		return workergraph.Output{}, err
	}
	path := filepath.Join(dir, "stdout.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return workergraph.Output{}, err
	}
	stderrPath := filepath.Join(dir, "stderr.log")
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return workergraph.Output{}, errors.Join(err, f.Close())
	}
	child, cancel := context.WithTimeout(ctx, time.Duration(spec.Timeout)*time.Second)
	defer cancel()
	// Bash sets both soft and hard limits when neither -S nor -H is supplied.
	// Its file-size unit is 1024 bytes; children inherit this per-file ceiling.
	// Pass model commands as an argument, never interpolate them in the wrapper.
	args := append(graphNodeEnvironment(workspace, dir), "bash", "-c", `ulimit -f 65536 || exit; exec bash -c "$1"`, "pwnmesh-run-graph", spec.Command)
	commandErr := process.RunStreams(child, dir, runDir, f, stderr, "env", args...)
	if err := errors.Join(f.Sync(), stderr.Sync(), f.Close(), stderr.Close()); err != nil {
		return workergraph.Output{}, errors.Join(commandErr, err)
	}
	if ctx.Err() != nil {
		return workergraph.Output{}, ctx.Err()
	}
	exitCode := 0
	if commandErr != nil {
		var exit *exec.ExitError
		if errors.As(commandErr, &exit) {
			exitCode = exit.ExitCode()
		} else {
			return workergraph.Output{}, fmt.Errorf("command failed; inspect %s and %s: %w", path, stderrPath, commandErr)
		}
	}
	artifact, preview, err := commandFile(ctx, dir, "stdout.log", 8000)
	if err != nil {
		return workergraph.Output{}, errors.Join(commandErr, err)
	}
	stderrArtifact, stderrPreview, err := commandFile(ctx, dir, "stderr.log", 8000)
	if err != nil {
		return workergraph.Output{}, errors.Join(commandErr, err)
	}
	value, _ := json.Marshal(Output{Stdout: preview, OutputPath: path, Stderr: stderrPreview, StderrPath: stderrPath, ExitCode: exitCode})
	out := workergraph.Output{Value: value, Artifacts: []workergraph.Artifact{artifact}}
	// Keep declared file offsets stable, while retaining diagnostics on failure.
	if commandErr != nil {
		out.Artifacts = append(out.Artifacts, stderrArtifact)
		return out, fmt.Errorf("command exited %d; inspect %s and %s: %w", exitCode, path, stderrPath, commandErr)
	}
	for _, name := range spec.Artifacts {
		artifact, _, err := commandFile(ctx, dir, name, 0)
		if err != nil {
			out.Artifacts = append(out.Artifacts, stderrArtifact)
			return out, err
		}
		out.Artifacts = append(out.Artifacts, artifact)
	}
	out.Artifacts = append(out.Artifacts, stderrArtifact)
	return out, nil
}

func graphNodeEnvironment(workspace, dir string) []string {
	return []string{"PWNMESH_WORKSPACE=" + workspace, "PWNMESH_NODE_DIR=" + dir, "PWNMESH_DEPENDENCIES=" + filepath.Join(dir, "dependencies.json")}
}

func commandNewFile(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(raw)
	return errors.Join(writeErr, f.Sync(), f.Close())
}

// Stream full artifacts into the digest while retaining only a bounded preview.
func commandFile(ctx context.Context, dir, name string, previewLimit int) (workergraph.Artifact, string, error) {
	artifact, preview, err := inspectCommandFile(ctx, dir, name, previewLimit)
	return artifact, preview.Text, err
}

type commandPreview struct {
	Text     string
	Complete bool
}

// Conditions consume the same retained bytes that are verified, not the lossy
// preview. Keep only a chunk and its overlapping suffix, and finish hashing even
// after a match so a changed file cannot authorize a downstream side effect.
func commandOutputContains(ctx context.Context, dir string, out workergraph.Output, contains string) (bool, error) {
	var value Output
	if json.Unmarshal(out.Value, &value) != nil || value.ExitCode != 0 || value.OutputPath != filepath.Join(dir, "stdout.log") || len(out.Artifacts) == 0 {
		return false, errors.New("invalid condition dependency output")
	}
	needle := []byte(contains)
	matched := len(needle) == 0
	window := make([]byte, 0, (64<<10)+len(needle))
	artifact, preview, err := inspectCommandFileChunks(ctx, dir, "stdout.log", 8000, func(chunk []byte) {
		if matched {
			return
		}
		window = append(window, chunk...)
		matched = bytes.Contains(window, needle)
		keep := min(len(window), len(needle)-1)
		copy(window, window[len(window)-keep:])
		window = window[:keep]
	})
	if err != nil {
		return false, err
	}
	if artifact != out.Artifacts[0] || preview.Text != value.Stdout {
		return false, errors.New("condition dependency stdout SHA-256 or preview changed")
	}
	return matched, nil
}

func inspectCommandFile(ctx context.Context, dir, name string, previewLimit int) (workergraph.Artifact, commandPreview, error) {
	return inspectCommandFileChunks(ctx, dir, name, previewLimit, nil)
}

func inspectCommandFileChunks(ctx context.Context, dir, name string, previewLimit int, consume func([]byte)) (workergraph.Artifact, commandPreview, error) {
	path := filepath.Join(dir, name)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return workergraph.Artifact{}, commandPreview{}, fmt.Errorf("command artifact %q must exist and not use symlinks", path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return workergraph.Artifact{}, commandPreview{}, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return workergraph.Artifact{}, commandPreview{}, errors.New("command artifact must be a regular file no larger than 64 MiB")
	}
	digest := sha256.New()
	buffer := make([]byte, 64<<10)
	preview := make([]byte, 0, previewLimit)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return workergraph.Artifact{}, commandPreview{}, err
		}
		n, readErr := f.Read(buffer)
		if n > 0 {
			size += int64(n)
			if size > 64<<20 {
				return workergraph.Artifact{}, commandPreview{}, errors.New("command artifact grew beyond 64 MiB")
			}
			digest.Write(buffer[:n])
			preview = append(preview, buffer[:min(n, previewLimit-len(preview))]...)
			if consume != nil {
				consume(buffer[:n])
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return workergraph.Artifact{}, commandPreview{}, readErr
		}
	}
	after, err := f.Stat()
	if err != nil || info.Size() != size || after.Size() != size || !info.ModTime().Equal(after.ModTime()) {
		return workergraph.Artifact{}, commandPreview{}, errors.New("command artifact changed while reading")
	}
	text := strings.ToValidUTF8(string(preview), "�")
	if previewLimit > 0 && info.Size() > int64(previewLimit) {
		text += "\n[Preview truncated; read the retained file for full output.]"
	}
	return workergraph.Artifact{Path: path, SHA256: hex.EncodeToString(digest.Sum(nil))}, commandPreview{Text: text, Complete: size == int64(len(preview)) && utf8.Valid(preview) && strings.TrimSpace(text) != ""}, nil
}
