//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"xloom/internal/artifactcheck"
	"xloom/internal/board"
)

// Walk from an open workspace descriptor: neither parent nor final symlinks
// can redirect a queued repair to a different artifact. Pipes/devices fail
// before reading and cannot stall a deterministic preflight.
func readRepairTarget(ctx context.Context, workspace, target string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(workspace, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return nil, errors.New("repair target must be a file inside the execution workspace")
	}
	fd, err := unix.Open(workspace, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(rel, "/")
	for n, component := range parts {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if n < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, openErr := unix.Openat(fd, component, flags, 0)
		unix.Close(fd)
		if openErr != nil {
			return nil, openErr
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), target)
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const limit = 4 << 20
	if !before.Mode().IsRegular() || before.Size() > limit {
		return nil, errors.New("repair target must be a regular file at most 4 MiB")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if len(raw) > limit || int64(len(raw)) != before.Size() || after.Size() != before.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("repair target changed while reading")
	}
	return raw, ctx.Err()
}

func inspectRepair(ctx context.Context, j Job) (RepairCheck, []byte, error) {
	if j.Repair == nil {
		return RepairCheck{}, nil, errors.New("missing repair contract")
	}
	raw, err := readRepairTarget(ctx, j.Workspace, j.Repair.Path)
	if err != nil {
		return RepairCheck{}, nil, err
	}
	result, err := artifactcheck.Evaluate(*j.Repair, raw)
	check := RepairCheck{Result: result, Path: j.Repair.Path, ExpectedSHA256: j.Repair.SHA256, Outcome: "execute", BlankContent: strings.TrimSpace(string(raw)) == ""}
	if result.Satisfied {
		check.Outcome = "noop"
	} else if result.SHA256 != j.Repair.SHA256 {
		check.Outcome = "stale"
	}
	return check, raw, err
}

func repairSuccess(check RepairCheck, refs []board.EvidenceRef, now time.Time) Result {
	description := "Repair requirements passed after execution"
	if check.Outcome == "noop" {
		description = "Repair not needed: the current artifact already satisfies every required check; no model call or repair tool was executed"
	}
	raw, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": map[string]any{
		"description": description, "scope": fmt.Sprintf("%s at SHA-256 %s; finite checks only", check.Path, check.SHA256),
		"observed_at": now.UTC().Format(time.RFC3339Nano), "evidence": refs,
	}}})
	return Result{Type: "result", Status: "success", Text: string(raw), RepairCheck: &check}
}

// Acceptance reopens the target and retains both original bytes and the
// deterministic receipt. Passing a finite predicate is not a semantic finding.
func finishRepair(ctx context.Context, j Job, dir string, r Result, initial *RepairCheck, now time.Time) (Result, error) {
	check, raw, err := inspectRepair(ctx, j)
	if err != nil {
		return r, err
	}
	if !check.Satisfied {
		return r, errors.New("repair requirements still fail at acceptance")
	}
	if initial == nil {
		return r, errors.New("repair is missing its preflight receipt")
	}
	if initial.Outcome == "noop" && initial.SHA256 != check.SHA256 {
		return r, errors.New("repair target changed after the no-op inspection")
	}
	check.Outcome = initial.Outcome
	if check.Outcome != "noop" && check.Outcome != "execute" {
		return r, errors.New("stale repair cannot be accepted")
	}
	evidenceDir := filepath.Join(dir, "evidence")
	if err := os.MkdirAll(evidenceDir, 0700); err != nil {
		return r, err
	}
	info, err := os.Lstat(evidenceDir)
	if err != nil || !info.IsDir() {
		return r, errors.New("repair evidence directory must not be a symlink")
	}
	var refs []board.EvidenceRef
	{
		ref, err := retainEvidenceBytes(ctx, j.RunID, evidenceDir, raw)
		if err != nil {
			return r, err
		}
		ref.Excerpt = strings.TrimLeftFunc(ref.Excerpt, unicode.IsSpace)
		if len(ref.Excerpt) > 4096 {
			ref.Excerpt = ref.Excerpt[:4096]
			for !utf8.ValidString(ref.Excerpt) {
				ref.Excerpt = ref.Excerpt[:len(ref.Excerpt)-1]
			}
		}
		// Blank artifacts still have a retained SHA-256 snapshot. The receipt
		// is their readable evidence; do not invent a nonblank original excerpt.
		if !check.BlankContent {
			refs = append(refs, ref)
		}
	}
	receipt, _ := json.Marshal(check)
	ref, err := retainEvidenceBytes(ctx, j.RunID, evidenceDir, receipt)
	if err != nil {
		return r, err
	}
	refs = append(refs, ref)
	return repairSuccess(check, refs, now), nil
}
