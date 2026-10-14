//go:build linux

package worker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"pwnmesh/internal/agent"
)

func TestPhaseTransitionSaveFailureKeepsCommittedState(t *testing.T) {
	for _, transition := range []string{"conclusion", "repair", "continuation", "conclusion input"} {
		t.Run(transition, func(t *testing.T) {
			job, dir := outcomeJob(t, "explore"), t.TempDir()
			job.Budget.Timeout, job.Budget.ConcludeTimeout = 60, 30
			now := time.Now()
			runtime, err := restoreSession(job, Options{RunDir: dir, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.journal.Close()
			message := agent.Text("assistant", continueOutput)
			message.Sequence = 2
			runtime.loop = &agent.Loop{TaskPrompt: "pinned task", History: []agent.Message{agent.Text("user", "pinned task"), message}, Checkpoint: &agent.ContextCheckpoint{Version: agent.ContextCheckpointVersion, LastSequence: 2}, SaveState: runtime.saveAgentState, Emit: runtime.emit}
			if transition == "conclusion input" {
				// Recovery after the first boundary save must preserve that
				// boundary even when persisting its conservative input fails.
				runtime.state.Concluding = true
				runtime.state.ConcludeStartedAt, runtime.state.ConcludeDeadline = now, now.Add(30*time.Second)
				runtime.loop.Concluding = true
			}
			if err := os.WriteFile(filepath.Join(dir, "output-proof.txt"), []byte("must not be read before the boundary commits"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := runtime.save(runtime.loop.History); err != nil {
				t.Fatal(err)
			}
			before := runtime.state
			beforeHistory := append([]agent.Message(nil), runtime.loop.History...)
			beforeDisk, err := os.ReadFile(filepath.Join(dir, "session.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.journal.Close(); err != nil {
				t.Fatal(err)
			}
			phases := &sessionPhases{runtime: runtime, ctx: context.Background(), phaseCtx: context.Background(), endCancel: func() {}}
			defer func() { phases.endCancel() }()
			switch transition {
			case "conclusion", "conclusion input":
				_, err = phases.startConclusion()
			case "repair":
				_, err = phases.startRepair(context.Background(), &outputFailure{Reason: "invalid_contract", Detail: "test"})
			case "continuation":
				_, err = phases.continueExecution()
			}
			if err == nil {
				t.Fatal("phase transition ignored the failed save")
			}
			afterDisk, readErr := os.ReadFile(filepath.Join(dir, "session.json"))
			if readErr != nil || string(afterDisk) != string(beforeDisk) {
				t.Fatalf("failed transition changed the committed file: %v", readErr)
			}
			if !reflect.DeepEqual(runtime.state, before) {
				t.Fatal("failed phase save consumed an allowance or changed the in-memory committed state")
			}
			if runtime.loop.Concluding != before.Concluding || runtime.loop.Repairing != before.Repairing || runtime.loop.RepairPrompt != before.RepairPrompt || runtime.loop.ConclusionPrompt != before.ConclusionPrompt || !reflect.DeepEqual(runtime.loop.History, beforeHistory) {
				t.Fatal("failed phase save changed the Loop or appended a phase instruction")
			}
			if _, err := os.Stat(filepath.Join(dir, "evidence")); !os.IsNotExist(err) {
				t.Fatalf("rejected transition collected new output artifacts: %v", err)
			}
		})
	}
}
