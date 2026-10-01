//go:build linux

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

// Share only the scripted model's wire protocol; each acceptance test keeps
// its own scenario, transitions and assertions.
type scriptedModelReply struct {
	w    http.ResponseWriter
	turn int
}

func (r scriptedModelReply) respond(block agent.Block, stop string) {
	r.w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(r.w).Encode(map[string]any{"role": "assistant", "content": []agent.Block{block}, "stop_reason": stop})
}

func (r scriptedModelReply) call(name string, input any) {
	raw, _ := json.Marshal(input)
	r.respond(agent.Block{Type: "tool_use", ID: fmt.Sprintf("call-%d", r.turn), Name: name, Input: raw}, "tool_use")
}

func (r scriptedModelReply) action(op, key string, payload any) {
	r.call("graph_action", map[string]any{"op": op, "idempotency_key": key, "payload": payload})
}

func scriptedJob(ctx context.Context, store *board.Store, project, run string) (worker.Job, error) {
	var execution board.Execution
	err := store.Do(ctx, func(tx *board.Tx) error {
		var err error
		execution, err = tx.Execution(project, run)
		return err
	})
	var job worker.Job
	if err == nil {
		err = json.Unmarshal(execution.Job, &job)
	}
	return job, err
}

func scriptedToolResult(messages []agent.Message, id string, wantError bool) (string, error) {
	// Runtime review/update context may follow a settled result. Bind to its
	// call ID rather than assuming that the final message contains the result.
	for n := len(messages) - 1; n >= 0; n-- {
		for _, block := range messages[n].Content {
			if block.Type != "tool_result" || block.ToolUseID != id {
				continue
			}
			if block.IsError != wantError {
				return "", fmt.Errorf("tool %s unexpected error=%t: %s", id, block.IsError, block.Content)
			}
			var text string
			err := json.Unmarshal(block.Content, &text)
			return text, err
		}
	}
	return "", fmt.Errorf("missing settled response to %s", id)
}
