//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	board "pwnmesh/internal/board/model"
)

func configureRoleTools(t *testing.T, job Job, options *Options) *roleRuntime {
	t.Helper()
	runtime, err := newRoleRuntime(job, *options, nil)
	if err != nil {
		t.Fatal(err)
	}
	options.Tools = runtime.tools
	return runtime
}

// A caller can reuse or change its configuration while already assembled tools
// continue to use their own bridge and graph cursor.
func TestRoleRuntimeOwnsBridgeAndGraphState(t *testing.T) {
	job := Job{Kind: "reason", RunID: "owned", GraphRPC: true,
		Decision: &board.DecisionContext{Version: 2, StateVersion: strings.Repeat("a", 64)}}
	config := Options{RunDir: t.TempDir()}
	calls := 0
	config.Output = &draftTestBridge{dir: config.RunDir, handle: func(GraphRequest) (any, error) {
		calls++
		return map[string]any{"state_version": strings.Repeat("b", 64)}, nil
	}}
	first := configureRoleTools(t, job, &config)
	firstRead := first.tools[0]
	second, err := newRoleRuntime(job, Options{RunDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	config.RunDir, config.Output, config.Tools = t.TempDir(), io.Discard, nil
	if _, err := firstRead.Execute(context.Background(), json.RawMessage(`{"section":"overview"}`)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first.graph.Version != strings.Repeat("b", 64) || second.graph.Version != strings.Repeat("a", 64) {
		t.Fatalf("tool bridge or graph state escaped its runtime: calls=%d first=%s second=%s", calls, first.graph.Version, second.graph.Version)
	}
}

func TestSessionSaveUsesSuppliedCheckpointAndRollsBackRejectedSave(t *testing.T) {
	job, dir := outcomeJob(t, "explore"), t.TempDir()
	runtime, err := restoreSession(job, Options{RunDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.journal.Close() })
	runtime.roles = &roleRuntime{graph: graphRuntimeState{Version: "accepted"}}
	runtime.loop = &agent.Loop{Checkpoint: &agent.ContextCheckpoint{Version: agent.ContextCheckpointVersion, LastSequence: 99}, TaskPrompt: "original task"}
	supplied := &agent.ContextCheckpoint{Version: agent.ContextCheckpointVersion, LastSequence: 3}
	if err := runtime.saveAgentState(nil, supplied); err != nil {
		t.Fatal(err)
	}
	// Runtime-triggered saves must detach the checkpoint too, since another
	// Agent append can advance the live sequence before its next save succeeds.
	runtime.loop.Checkpoint = supplied
	if err := runtime.save(nil); err != nil {
		t.Fatal(err)
	}
	runtime.loop.Checkpoint.LastSequence = 99
	if runtime.state.ContextCheckpoint.LastSequence != 3 {
		t.Fatal("runtime-only save retained the live checkpoint pointer")
	}
	before, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved session
	if err := json.Unmarshal(before, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ContextCheckpoint.LastSequence != 3 || saved.GraphVersion != "accepted" {
		t.Fatalf("save ignored its checkpoint or graph snapshot: %+v", saved)
	}
	// Live graph progress has no pointer into the last saved session snapshot.
	runtime.roles.graph = graphRuntimeState{Version: "pending", Conflict: "pending conflict"}
	if runtime.state.GraphVersion != "accepted" || runtime.state.DecisionConflict != "" {
		t.Fatal("role mutation rewrote the saved session outside persistence")
	}
	old := runtime.state
	if err := runtime.journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.saveAgentState([]agent.Message{agent.Text("user", "not committed")}, &agent.ContextCheckpoint{Version: agent.ContextCheckpointVersion, LastSequence: 4}); err == nil {
		t.Fatal("closed journal accepted a checkpoint")
	}
	after, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || !reflect.DeepEqual(runtime.state, old) {
		t.Fatal("failed save changed the durable session or its in-memory snapshot")
	}
}

func TestRoleRecoveryAdaptsHistoricalPlannerOnlyAfterReceipt(t *testing.T) {
	for _, committed := range []bool{false, true} {
		roles := &roleRuntime{graph: graphRuntimeState{Version: "bound-version", Conflict: "historical conflict"}}
		roles.decision = &decisionDraft{request: func(context.Context, GraphRequest) (string, error) {
			raw, _ := json.Marshal(board.DecisionReceipt{Committed: committed})
			return string(raw), nil
		}}
		_, done, err := roles.recover(context.Background(), Job{Kind: "reason"}, false)
		if err != nil || done != committed {
			t.Fatalf("receipt recovery changed authority: done=%t err=%v", done, err)
		}
		if committed {
			if roles.graph.Conflict != "historical conflict" || roles.decision.reread {
				t.Fatal("committed receipt was adapted as an uncommitted draft")
			}
		} else if roles.graph.Conflict != "" || !roles.decision.reread {
			t.Fatal("uncommitted historical conflict did not require a fresh evidence read")
		}
	}
	roles := &roleRuntime{graph: graphRuntimeState{Conflict: "historical conflict"}, decision: &decisionDraft{request: func(context.Context, GraphRequest) (string, error) { return "", errors.New("bridge unavailable") }}}
	if _, _, err := roles.recover(context.Background(), Job{Kind: "reason"}, true); err == nil || roles.graph.Conflict == "" {
		t.Fatal("failed receipt read granted a fresh draft")
	}
}
