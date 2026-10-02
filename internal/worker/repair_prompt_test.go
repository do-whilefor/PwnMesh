package worker

import (
	"strings"
	"testing"

	"pwnmesh/internal/agent"
)

func TestRepairPromptTracksFailureAndCurrentPhase(t *testing.T) {
	job := Job{Kind: "explore", ResultContractVersion: 2, GraphRPC: true}
	for _, concluding := range []bool{false, true} {
		for _, stop := range []string{"end_turn", "max_tokens", "length"} {
			message := agent.Text("assistant", `{"accepted":true`)
			message.StopReason = stop
			problem := outputProblem(job, concluding, message)
			if problem == nil {
				t.Fatal("invalid or truncated output did not require repair")
			}
			prompt, err := repairInstruction(job, concluding, 1, problem)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(prompt, "Rewrite the truncated response") != (stop != "end_turn") || strings.Contains(prompt, "report continue") == concluding {
				t.Fatalf("repair guidance ignored failure or phase: conclude=%t stop=%s prompt=%s", concluding, stop, prompt)
			}
			for _, required := range []string{"All tools are disabled", "one complete JSON object", "original task contract", "current phase restrictions", "Use only existing evidence", "proof for completion", "never force accepted:true", "report incomplete", "do not append a suffix"} {
				if !strings.Contains(prompt, required) {
					t.Fatalf("repair omitted %q", required)
				}
			}
		}
	}
	message := agent.Message{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "call", Name: "read"}}}
	problem := outputProblem(job, true, message)
	if problem == nil || problem.Reason != "unexpected_tool_call" {
		t.Fatal("pure-output tool call was misclassified")
	}
	prompt, err := repairInstruction(job, true, 1, problem)
	if err != nil || strings.Contains(prompt, "Rewrite the truncated response") {
		t.Fatal("non-truncated tool call received truncation guidance")
	}
}
