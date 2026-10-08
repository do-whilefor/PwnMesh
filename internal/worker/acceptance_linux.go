//go:build linux

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

type acceptanceArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type acceptanceOutput struct {
	Value     json.RawMessage      `json:"value,omitempty"`
	Artifacts []acceptanceArtifact `json:"artifacts,omitempty"`
}

// A running receipt delegates recovery to the bound session. Accepted receipts
// retain one result and its original hashes; deterministic acceptance failures
// are terminal and never authorize another session attempt.
type acceptanceReceipt struct {
	SchemaVersion int              `json:"schema_version"`
	RunID         string           `json:"run_id"`
	InputSHA256   string           `json:"input_sha256"`
	Status        string           `json:"status"`
	Output        acceptanceOutput `json:"output"`
	Error         string           `json:"error,omitempty"`
}

var acceptanceRunID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// The caller owns the original graph-directory lock, including during migration.
func acceptWorkerSession(ctx context.Context, runID string, input json.RawMessage, dir string, call func(context.Context) (acceptanceOutput, error), verify func(context.Context, acceptanceOutput) error) (acceptanceOutput, error) {
	if !acceptanceRunID.MatchString(runID) || len(input) > 1<<20 || !json.Valid(input) {
		return acceptanceOutput{}, errors.New("acceptance requires a run identity and bounded JSON input")
	}
	digest := sha256.Sum256(input)
	receipt := acceptanceReceipt{SchemaVersion: 1, RunID: runID, InputSHA256: hex.EncodeToString(digest[:]), Status: "running"}
	path := filepath.Join(dir, "acceptance.json")
	raw, err := readEvidenceFile(ctx, path, 2<<20)
	if err == nil {
		var saved acceptanceReceipt
		if json.Unmarshal(raw, &saved) != nil || saved.SchemaVersion != receipt.SchemaVersion || saved.RunID != runID || saved.InputSHA256 != receipt.InputSHA256 {
			return acceptanceOutput{}, errors.New("acceptance checkpoint identity or input mismatch")
		}
		switch saved.Status {
		case "accepted":
			if saved.Error != "" {
				return acceptanceOutput{}, errors.New("invalid accepted receipt")
			}
			if err := verifyAcceptance(ctx, dir, saved.Output, verify); err != nil {
				return acceptanceOutput{}, err
			}
			return saved.Output, nil
		case "failed":
			if saved.Error == "" || len(saved.Output.Value) != 0 || len(saved.Output.Artifacts) != 0 {
				return acceptanceOutput{}, errors.New("invalid failed receipt")
			}
			return acceptanceOutput{}, errors.New(saved.Error)
		case "running":
			if saved.Error != "" || len(saved.Output.Value) != 0 || len(saved.Output.Artifacts) != 0 {
				return acceptanceOutput{}, errors.New("invalid running receipt")
			}
		default:
			return acceptanceOutput{}, errors.New("invalid acceptance status")
		}
	} else if !os.IsNotExist(err) {
		return acceptanceOutput{}, err
	} else if _, legacyErr := os.Lstat(filepath.Join(dir, "graph.json")); legacyErr == nil {
		// Do not write a running receipt before migration: a crash must return to
		// the legacy checkpoint, whose agent may already have been accepted.
		out, err := resumeLegacyAcceptance(ctx, runID, input, dir, call, verify)
		if err != nil {
			return acceptanceOutput{}, err
		}
		if err := verifyAcceptance(ctx, dir, out, verify); err != nil {
			return acceptanceOutput{}, err
		}
		receipt.Status, receipt.Output = "accepted", out
		return out, saveAcceptance(dir, receipt)
	} else if !os.IsNotExist(legacyErr) {
		return acceptanceOutput{}, legacyErr
	}
	if err := ctx.Err(); err != nil {
		return acceptanceOutput{}, err
	}
	if err := saveAcceptance(dir, receipt); err != nil {
		return acceptanceOutput{}, err
	}
	out, err := invokeAcceptance(ctx, func() (acceptanceOutput, error) { return call(ctx) })
	if err == nil {
		err = verifyAcceptance(ctx, dir, out, verify)
	}
	if err != nil {
		if errors.Is(err, ErrInterrupted) || ctx.Err() != nil {
			return acceptanceOutput{}, err
		}
		receipt.Status, receipt.Error = "failed", err.Error()
		return acceptanceOutput{}, errors.Join(err, saveAcceptance(dir, receipt))
	}
	receipt.Status, receipt.Output = "accepted", out
	return out, saveAcceptance(dir, receipt)
}

func verifyAcceptance(ctx context.Context, dir string, out acceptanceOutput, verify func(context.Context, acceptanceOutput) error) error {
	var result Result
	if len(out.Value) == 0 || len(out.Value) > 1<<20 || json.Unmarshal(out.Value, &result) != nil || (result.Status != "success" && result.Status != "failed") || result.Retryable || len(out.Artifacts) == 0 || len(out.Artifacts) > 64 || out.Artifacts[0].Path != filepath.Join(filepath.Dir(dir), "session.json") {
		return errors.New("invalid or oversized acceptance output")
	}
	for _, artifact := range out.Artifacts {
		digest, err := hex.DecodeString(artifact.SHA256)
		if !filepath.IsAbs(artifact.Path) || len(artifact.Path) > 4096 || err != nil || len(digest) != sha256.Size {
			return errors.New("acceptance artifact requires an absolute path and SHA-256")
		}
	}
	_, err := invokeAcceptance(ctx, func() (acceptanceOutput, error) { return acceptanceOutput{}, verify(ctx, out) })
	return err
}

func invokeAcceptance(ctx context.Context, fn func() (acceptanceOutput, error)) (out acceptanceOutput, err error) {
	defer func() {
		if value := recover(); value != nil {
			err = errors.Join(ErrInterrupted, fmt.Errorf("acceptance callback panic: %v", value))
		}
		if ctx.Err() != nil {
			err = errors.Join(ErrInterrupted, ctx.Err(), err)
		}
	}()
	return fn()
}

func saveAcceptance(dir string, receipt acceptanceReceipt) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".acceptance-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(raw)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, "acceptance.json")); err != nil {
		return err
	}
	return syncDirectory(dir)
}
