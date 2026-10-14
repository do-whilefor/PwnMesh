//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"pwnmesh/internal/agent"
)

func TestCommittedTranscriptBindsResultAndExcludesCrashTail(t *testing.T) {
	job, dir := outcomeJob(t, "explore"), t.TempDir()
	runtime, err := restoreSession(job, Options{RunDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.journal.file.Close()
	message := agent.Text("assistant", "committed observation")
	message.Sequence = 1
	if err := runtime.journal.append(agent.Event{Type: "message_end", Message: &message}); err != nil {
		t.Fatal(err)
	}
	runtime.state.Result = &Result{Type: "result", Status: "success", Text: completedOutput}
	if err := runtime.saveAgentState([]agent.Message{message}, &agent.ContextCheckpoint{Version: agent.ContextCheckpointVersion, LastSequence: 1}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := openCommittedTranscript(dir, runtime.state.Identity, raw, *runtime.state.Result)
	if err != nil {
		t.Fatal(err)
	}
	tail := agent.Text("assistant", "uncommitted observation")
	tail.Sequence = 2
	if err := runtime.journal.append(agent.Event{Type: "message_end", Message: &tail}); err != nil {
		t.Fatal(err)
	}
	for _, visit := range []func(func(agent.Message) error) error{transcript.visitCurrent, func(consume func(agent.Message) error) error {
		return transcript.visitOriginal(context.Background(), consume)
	}} {
		count := 0
		if err := visit(func(m agent.Message) error {
			count++
			if m.Sequence != 1 || m.Text() != message.Text() {
				t.Fatal("reader admitted an uncommitted message")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("reader lost its bound message: %d", count)
		}
	}
	changed := runtime.state.Identity
	changed.RunID = "another-run"
	if _, err := openCommittedTranscript(dir, changed, raw, *runtime.state.Result); err == nil {
		t.Fatal("reader admitted another run identity")
	}
	result := *runtime.state.Result
	result.Text = "another result"
	if _, err := openCommittedTranscript(dir, runtime.state.Identity, raw, result); err == nil {
		t.Fatal("reader admitted another terminal result")
	}
	// Tampering inside the committed prefix is distinct from an ignored tail.
	encoded, _ := json.Marshal(agent.Event{Type: "message_end", Message: &tail})
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), append(encoded, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := transcript.visitOriginal(context.Background(), func(agent.Message) error { return nil }); err == nil {
		t.Fatal("reader accepted changed committed journal bytes")
	}
}
