//go:build linux

package process

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// Publish a complete marker or none: a crashed writer must not leave a partial
// record that prevents later cleanup from reading the command's ownership.
func publishIdentity(path string, id identity) error {
	raw, err := json.Marshal(id)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".process-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(raw)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// The target cannot run until its process group has been published. If the
// worker dies before publication, the pipe closes and the shell exits on EOF.
// Keeping the gate in the child also covers a delayed fork/exec after shutdown.
func startCommand(cmd *exec.Cmd, persist func(int) error) error {
	reader, gate, err := os.Pipe()
	if err != nil {
		return err
	}
	defer reader.Close()
	defer gate.Close()
	command := cmd.Path // Preserve exec.Command's executable lookup.
	args := cmd.Args
	cmd.Path = "/bin/sh"
	cmd.Args = append([]string{"sh", "-c", `IFS= read -r gate <&3 || exit 125; exec 3<&-; [ "$gate" = go ] || exit 125; exec "$@"`, "pwnmesh-command", command}, args[1:]...)
	cmd.ExtraFiles = []*os.File{reader}
	if err := cmd.Start(); err != nil {
		return err
	}
	reader.Close()
	if err := persist(cmd.Process.Pid); err != nil {
		return err
	}
	_, err = io.WriteString(gate, "go\n")
	return err
}
