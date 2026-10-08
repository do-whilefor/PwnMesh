//go:build linux

package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/process"
)

// Run executes each role with the shared Agent Loop and records Execute
// acceptance in a durable receipt after the session and its evidence are verified.
func Run(ctx context.Context, j Job, o Options) (Result, error) {
	if err := validateWorkerProtocol(j); err != nil {
		return Result{}, err
	}
	if j.Kind != "explore" || o.RunDir == "" || j.Workspace == "" {
		return runSession(ctx, j, o)
	}
	return runWorkerAcceptance(ctx, j, o, runSession)
}

// Keep acceptance testable without replacing the shared Agent Loop. The session
// owns same-run recovery; this boundary never schedules or replays tool calls.
func runWorkerAcceptance(ctx context.Context, j Job, o Options, sessionRun func(context.Context, Job, Options) (Result, error)) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	var err error
	o.RunDir, err = filepath.Abs(o.RunDir)
	if err != nil {
		return Result{}, err
	}
	j.Workspace, err = filepath.Abs(j.Workspace)
	if err != nil {
		return Result{}, err
	}
	if err = verifyInputFiles(ctx, j); err != nil {
		return Result{}, err
	}
	dir := filepath.Join(o.RunDir, "graph")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Result{}, err
	}
	unlock, err := process.Lock(dir)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	checkActive := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := process.CheckLaunch(o.RunDir); err != nil {
			return err
		}
		if process.Cancelled(o.RunDir) {
			return context.Canceled
		}
		return nil
	}
	if err := checkActive(ctx); err != nil {
		return Result{}, err
	}
	if o.Output == nil {
		o.Output = io.Discard
	}
	identity, err := identityFor(j, o.RunDir)
	if err != nil {
		return Result{}, err
	}
	input, err := json.Marshal(identity)
	if err != nil {
		return Result{}, err
	}
	// Session emits live tool/Agent events, but its terminal result is only a
	// candidate until its receipt has been durably accepted.
	output := &graphResultWriter{writer: o.Output}
	o.Output = output
	publish := func(r Result) (Result, error) {
		if err := checkActive(ctx); err != nil {
			return Result{}, err
		}
		r.Type = "result"
		return r, output.finish(r)
	}
	var result Result
	verify := func(ctx context.Context, out acceptanceOutput) error {
		if err := checkActive(ctx); err != nil {
			return err
		}
		for _, artifact := range out.Artifacts {
			bytes, err := readEvidenceFile(ctx, artifact.Path, 32<<20)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(bytes)
			if hex.EncodeToString(digest[:]) != artifact.SHA256 {
				return errors.New("graph artifact SHA-256 changed")
			}
		}
		if len(out.Value) == 0 {
			return nil
		}
		var r Result
		if json.Unmarshal(out.Value, &r) != nil {
			return errors.New("invalid graph result")
		}
		if r.Status != "success" {
			return nil
		}
		if j.ResultContractVersion >= 2 {
			parsed, err := parseOutput(j, r.Conclude, r.Text)
			if err != nil {
				return err
			}
			if len(parsed.FactPayload) > 0 {
				if _, err := graphInlineFactEvidence(ctx, j, o.RunDir, parsed.FactPayload); err != nil {
					return err
				}
			}
		}
		if j.Repair != nil {
			check, _, err := inspectRepair(ctx, j)
			if err != nil {
				return err
			}
			if r.RepairCheck == nil || !check.Satisfied || check.SHA256 != r.RepairCheck.SHA256 {
				return errors.New("repair target changed after graph acceptance; main Agent must reassess")
			}
		}
		return nil
	}
	call := func(ctx context.Context) (acceptanceOutput, error) {
		r, err := sessionRun(ctx, j, o)
		result = r
		if err != nil {
			return acceptanceOutput{}, errors.Join(ErrInterrupted, err)
		}
		if r.Retryable {
			return acceptanceOutput{}, errors.Join(ErrInterrupted, errors.New(r.Error))
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return acceptanceOutput{}, err
		}
		out := acceptanceOutput{Value: raw}
		// Session bytes and final retained evidence are immutable once accepted.
		sessionPath := filepath.Join(o.RunDir, "session.json")
		sessionBytes, err := readEvidenceFile(ctx, sessionPath, 32<<20)
		if err != nil {
			return out, err
		}
		sessionDigest := sha256.Sum256(sessionBytes)
		out.Artifacts = append(out.Artifacts, acceptanceArtifact{Path: sessionPath, SHA256: hex.EncodeToString(sessionDigest[:])})
		var artifacts []acceptanceArtifact
		if r.Status == "success" && r.RepairCheck != nil {
			artifacts = append(artifacts, acceptanceArtifact{Path: filepath.Join(o.RunDir, "evidence", r.RepairCheck.SHA256+".raw"), SHA256: r.RepairCheck.SHA256})
		}
		if r.Status == "success" {
			parsed, err := parseOutput(j, r.Conclude, r.Text)
			if err != nil {
				return out, err
			}
			if len(parsed.FactPayload) > 0 {
				refs, err := graphInlineFactEvidence(ctx, j, o.RunDir, parsed.FactPayload)
				if err != nil {
					return out, err
				}
				for _, ref := range refs {
					artifacts = append(artifacts, acceptanceArtifact{Path: ref.Path, SHA256: strings.TrimSuffix(filepath.Base(ref.Path), ".raw")})
				}
			}
			if parsed.FactID != "" {
				refs, err := graphPublishedFactEvidence(ctx, j, o.RunDir, identity, sessionBytes, r, parsed.FactID)
				if err != nil {
					return out, err
				}
				for _, ref := range refs {
					artifacts = append(artifacts, acceptanceArtifact{Path: ref.Path, SHA256: strings.TrimSuffix(filepath.Base(ref.Path), ".raw")})
				}
			}
		}
		seenPaths := map[string]bool{sessionPath: true}
		for _, artifact := range artifacts {
			if seenPaths[artifact.Path] {
				continue
			}
			seenPaths[artifact.Path] = true
			// Bind the original retained digest. Verification must reject a
			// later replacement, never certify it by hashing the newer bytes.
			out.Artifacts = append(out.Artifacts, artifact)
		}
		return out, nil
	}
	accepted, err := acceptWorkerSession(ctx, j.RunID, input, dir, call, verify)
	if err != nil {
		if activeErr := checkActive(ctx); activeErr != nil {
			return Result{}, activeErr
		}
		if errors.Is(err, ErrInterrupted) {
			if result.Retryable {
				return publish(result)
			}
			return Result{}, err
		}
		result = Result{Type: "result", Status: "failed", FailureKind: "graph_checkpoint", Error: err.Error()}
		return publish(result)
	}
	if err := json.Unmarshal(accepted.Value, &result); err != nil {
		return Result{}, err
	}
	return publish(result)
}

func graphInlineFactEvidence(ctx context.Context, j Job, runDir string, payload json.RawMessage) ([]board.EvidenceRef, error) {
	var fact struct {
		Evidence []board.EvidenceRef `json:"evidence"`
	}
	if err := json.Unmarshal(payload, &fact); err != nil {
		return nil, err
	}
	for _, ref := range fact.Evidence {
		if err := verifyEvidenceSnapshot(ctx, filepath.Join(runDir, "evidence"), j.RunID, ref, ref); err != nil {
			return nil, err
		}
	}
	return fact.Evidence, nil
}

// A final fact_id refers to a prior publication, not to inline final evidence.
// Resolve only the matching successful graph_action receipt in this run's
// durable session. Tool results are JSON strings containing service JSON.
// Model prose, arbitrary tool output and omitted receipts cannot establish this
// binding. Compacted history falls back to the bound original message journal.
func graphPublishedFactEvidence(ctx context.Context, j Job, runDir string, identity executionIdentity, raw []byte, result Result, factID string) ([]board.EvidenceRef, error) {
	var saved session
	if err := json.Unmarshal(raw, &saved); err != nil {
		return nil, errors.New("published fact requires a readable session")
	}
	if err := saved.validate(identity); err != nil {
		return nil, err
	}
	if saved.Result == nil || saved.Result.Status != "success" || saved.Result.Text != result.Text || j.Intent == nil {
		return nil, errors.New("published fact is not bound to the saved successful result")
	}
	pending := map[string]bool{}
	var selected []board.EvidenceRef
	var selectedRecord []byte
	consume := func(message agent.Message) error {
		for _, block := range message.Content {
			if message.Role == "assistant" && block.Type == "tool_use" {
				// A new use of an ID replaces its earlier association; never let
				// a non-graph tool inherit a previously authorized graph call.
				delete(pending, block.ID)
				var action board.StateAction
				if block.ID != "" && block.Name == "graph_action" && json.Unmarshal(block.Input, &action) == nil && action.Op == "fact" {
					pending[block.ID] = true
				}
				continue
			}
			if message.Role != "user" || block.Type != "tool_result" {
				continue
			}
			published := pending[block.ToolUseID]
			delete(pending, block.ToolUseID)
			if !published || block.IsError {
				continue
			}
			var text string
			var receipt board.StateActionResult
			if json.Unmarshal(block.Content, &text) != nil || json.Unmarshal([]byte(text), &receipt) != nil || receipt.Op != "fact" || receipt.ID != factID {
				continue
			}
			var fact board.FactRecord
			if json.Unmarshal(receipt.Result, &fact) != nil || fact.ID != factID || fact.Legacy || fact.Status != "valid" || fact.SourceStepID != j.Intent.ID || fact.RunID == "" || !currentEvidence(fact.RunID, j.RunID) || len(fact.Evidence) == 0 {
				return errors.New("published fact receipt lacks complete evidence from this Step and run")
			}
			record, err := json.Marshal(fact)
			if err != nil {
				return err
			}
			if selectedRecord != nil && !bytes.Equal(selectedRecord, record) {
				return errors.New("published fact has conflicting saved receipts")
			}
			for _, ref := range fact.Evidence {
				if err := verifyEvidenceSnapshot(ctx, filepath.Join(runDir, "evidence"), j.RunID, ref, ref); err != nil {
					return err
				}
			}
			selected, selectedRecord = fact.Evidence, record
		}
		return nil
	}
	for _, message := range saved.History {
		if err := consume(message); err != nil {
			return nil, err
		}
	}
	if len(selected) == 0 {
		pending = map[string]bool{}
		if err := graphJournalMessages(ctx, filepath.Join(runDir, "events.jsonl"), saved.Log, consume); err != nil {
			return nil, err
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("published fact evidence cannot be located in a successful graph_action receipt")
	}
	return selected, nil
}

// Read only the committed prefix; a later append or uncommitted crash tail is
// not evidence for this successful session. Memory is bounded per event, not
// by loading the run's entire raw history or scanning unrelated run files.
func graphJournalMessages(ctx context.Context, path string, checkpoint journalCheckpoint, consume func(agent.Message) error) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || checkpoint.Offset <= 0 || checkpoint.Offset > info.Size() || len(checkpoint.SHA256) != 64 {
		return errors.New("invalid committed journal boundary for published fact")
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], checkpoint.Offset-1); err != nil || last[0] != '\n' {
		return errors.New("published fact journal checkpoint is not a complete record")
	}
	digest := sha256.New()
	prefix := &io.LimitedReader{R: file, N: checkpoint.Offset}
	scanner := bufio.NewScanner(io.TeeReader(prefix, digest))
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var event agent.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		if event.Type == "message_end" && event.Message != nil {
			if err := consume(*event.Message); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if prefix.N != 0 || hex.EncodeToString(digest.Sum(nil)) != checkpoint.SHA256 {
		return errors.New("committed published fact journal SHA-256 mismatch")
	}
	return ctx.Err()
}

// graphResultWriter serializes complete JSONL frames. Encoders and graphRPC
// normally write whole frames; a frame split over successive writes is also
// retained until its newline. Events are never held until graph completion,
// because graph_request needs its live bridge response to make progress.
type graphResultWriter struct {
	mu       sync.Mutex
	writer   io.Writer
	pending  []byte
	err      error
	finished bool
}

func (w *graphResultWriter) writeFrame(frame []byte) error {
	n, err := w.writer.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	return err
}

func (w *graphResultWriter) emitEvent(frame []byte) error {
	var envelope struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(frame, &envelope) == nil && envelope.Type == "result" {
		return nil
	}
	return w.writeFrame(frame)
}

func (w *graphResultWriter) Write(raw []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	if w.finished {
		return 0, errors.New("graph output is already finalized")
	}
	size := len(raw)
	for len(raw) > 0 {
		end := bytes.IndexByte(raw, '\n')
		take := len(raw)
		if end >= 0 {
			take = end + 1
		}
		if len(w.pending)+take > 64<<20 {
			w.err = errors.New("graph output frame exceeds 64 MiB")
			return size - len(raw), w.err
		}
		w.pending = append(w.pending, raw[:take]...)
		raw = raw[take:]
		if end < 0 {
			break
		}
		w.err = w.emitEvent(w.pending)
		w.pending = w.pending[:0]
		if w.err != nil {
			return size - len(raw), w.err
		}
	}
	return size, nil
}

func (w *graphResultWriter) finish(result Result) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if w.finished {
		return errors.New("graph result was already emitted")
	}
	if len(bytes.TrimSpace(w.pending)) != 0 {
		if !json.Valid(w.pending) {
			return errors.New("graph output ended in an incomplete frame")
		}
		if w.err = w.emitEvent(append(w.pending, '\n')); w.err != nil {
			return w.err
		}
	}
	w.pending = nil
	w.finished = true
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	w.err = w.writeFrame(append(raw, '\n'))
	return w.err
}
