//go:build linux

package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCommandLaunchHelper(t *testing.T) {
	dir := os.Getenv("PWNMESH_TEST_PROCESS_HELPER")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := RegisterWorker(dir); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "echo launched > launched; exec sleep 30")
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := startCommand(cmd, func(pid int) error {
		if err := os.WriteFile(filepath.Join(dir, "ready"), []byte(strconv.Itoa(pid)), 0600); err != nil {
			return err
		}
		// Hold the exact launch-before-publication boundary until the parent
		// interrupts or crashes this worker. No group marker exists yet.
		time.Sleep(time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func processRunning(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(raw)[strings.LastIndexByte(string(raw), ')')+1:])
	return len(fields) > 0 && fields[0] != "Z"
}

func TestInterruptedLaunchCannotRunUnregisteredCommand(t *testing.T) {
	for _, stop := range []string{"interrupt", "force_cancel", "worker_crash"} {
		t.Run(stop, func(t *testing.T) {
			dir := t.TempDir()
			helper := exec.Command(os.Args[0], "-test.run=^TestCommandLaunchHelper$")
			helper.Env = append(os.Environ(), "PWNMESH_TEST_PROCESS_HELPER="+dir)
			helper.Stdout, helper.Stderr = io.Discard, io.Discard
			if err := helper.Start(); err != nil {
				t.Fatal(err)
			}
			pid := 0
			t.Cleanup(func() {
				if pid > 1 {
					syscall.Kill(-pid, syscall.SIGKILL)
				}
				helper.Process.Kill()
				helper.Wait()
			})
			deadline := time.Now().Add(3 * time.Second)
			for pid == 0 && time.Now().Before(deadline) {
				raw, _ := os.ReadFile(filepath.Join(dir, "ready"))
				pid, _ = strconv.Atoi(string(raw))
				if pid == 0 {
					time.Sleep(time.Millisecond)
				}
			}
			if pid <= 1 {
				t.Fatal("worker did not reach command publication")
			}
			var err error
			switch stop {
			case "interrupt":
				err = Interrupt(dir)
			case "force_cancel":
				err = Cancel(dir, true)
			case "worker_crash":
				err = helper.Process.Kill()
			}
			if err != nil {
				t.Fatal(err)
			}
			deadline = time.Now().Add(time.Second)
			for processRunning(pid) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if processRunning(pid) {
				t.Errorf("unregistered command %d survived %s", pid, stop)
			}
			if _, err := os.Stat(filepath.Join(dir, "launched")); !os.IsNotExist(err) {
				t.Errorf("target command ran before its ownership was published: %v", err)
			}
		})
	}
}

func TestCommandPublicationFailureCannotLaunchTarget(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "echo launched > launched")
	cmd.Dir = dir
	publicationErr := errors.New("ownership publication failed")
	err := startCommand(cmd, func(int) error {
		// Give an incorrectly ungated command time to finish before failing.
		time.Sleep(20 * time.Millisecond)
		return publicationErr
	})
	if !errors.Is(err, publicationErr) {
		t.Fatalf("publication failure = %v", err)
	}
	if cmd.Process != nil {
		defer cmd.Process.Kill()
		_ = cmd.Wait()
	}
	if _, err := os.Stat(filepath.Join(dir, "launched")); !os.IsNotExist(err) {
		t.Fatalf("target ran after failed ownership publication: %v", err)
	}
}

func TestCommandGateRejectsDeadChild(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("sh", "-c", "echo launched > launched")
	cmd.Dir = dir
	err := startCommand(cmd, func(int) error {
		if err := cmd.Process.Kill(); err != nil {
			return err
		}
		_ = cmd.Wait()
		return nil
	})
	if !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("dead command gate = %v, want broken pipe", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "launched")); !os.IsNotExist(err) {
		t.Fatalf("target ran before its gate opened: %v", err)
	}
}

func TestRunStreamsOwnershipWriteFailureDoesNotExecute(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(runDir, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	output := outputFile(t, dir, "output")
	err := RunStreams(context.Background(), dir, runDir, output, output, "sh", "-c", "echo launched > launched")
	if err == nil {
		t.Fatal("ownership write failure was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "launched")); !os.IsNotExist(err) {
		t.Fatalf("target ran without persistent ownership: %v", err)
	}
}
