package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
)

func TestProviderToolAndObserverBuffersCannotRewriteTranscript(t *testing.T) {
	response := Message{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "call", Name: "read", Input: json.RawMessage(`{"value":"original"}`)}}, Usage: &Usage{InputTokens: 10}}
	var retained []Message
	var saved []Message
	var checkpoint *ContextCheckpoint
	turns, executed := 0, 0
	loop := &Loop{Tools: []Tool{{Definition: Definition{Name: "read", Schema: json.RawMessage(`{"type":"object"}`)}, Execute: func(_ context.Context, raw json.RawMessage) (string, error) {
		copy(raw, []byte(`{"value":"modified"}`))
		executed++
		return "original evidence", nil
	}}}}
	loop.Provider = observedProvider{generate: func(_ context.Context, messages []Message, defs []Definition, _ Emit) (Message, error) {
		turns++
		if turns == 1 {
			retained = messages
			messages[0].Content[0].Text = "provider private scratch"
			defs[0].Schema[0] = '!'
			return response, nil
		}
		if messages[0].Text() != "task" || string(messages[1].Content[0].Input) != `{"value":"original"}` || messages[1].Usage.InputTokens != 10 {
			t.Fatalf("request observed overwritten task, arguments or usage: %+v", messages)
		}
		retained[0].Content[0].Text = "another agent"
		response.Content[0].Name = "another agent's response buffer"
		response.Usage.InputTokens = 999
		saved[0].Content[0].Text = "saved snapshot mutation"
		checkpoint.LastSequence = 999
		return Text("assistant", "done"), nil
	}}
	loop.SaveState = func(history []Message, next *ContextCheckpoint) error {
		saved, checkpoint = history, next
		return nil
	}
	loop.Emit = func(event Event) {
		if event.Message != nil && event.Message.Role == "assistant" && len(event.Message.Content) > 0 {
			event.Message.Content[0].Name = "observer scratch"
			if event.Message.Usage != nil {
				event.Message.Usage.InputTokens = 777
			}
		}
	}
	if text, err := loop.Run(context.Background(), "task"); err != nil || text != "done" || executed != 1 {
		t.Fatalf("isolated run failed: text=%q err=%v executed=%d", text, err, executed)
	}
	call := loop.History[1]
	if loop.History[0].Text() != "task" || call.Content[0].Name != "read" || string(call.Content[0].Input) != `{"value":"original"}` || call.Usage.InputTokens != 10 || loop.Checkpoint.LastSequence != 4 || !json.Valid(loop.Tools[0].Schema) {
		t.Fatalf("transcript or tool registry aliased external buffers: history=%+v checkpoint=%+v", loop.History, loop.Checkpoint)
	}
}

func TestConcurrentLoopsOwnSeedHistoryAndRecoveryCheckpoint(t *testing.T) {
	seed := make([]Message, 1, 16)
	seed[0] = Text("user", "shared immutable origin")
	seed[0].Sequence = 1
	checkpoint := &ContextCheckpoint{Version: ContextCheckpointVersion, LastSequence: 1, LastCompaction: &CompactionCheckpoint{Quotes: []SummaryQuote{{Text: "original quote"}}, Usage: &Usage{InputTokens: 3}}}
	var wg sync.WaitGroup
	for _, name := range []string{"agent-a", "agent-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop := &Loop{History: seed, Checkpoint: checkpoint, Provider: observedProvider{generate: func(_ context.Context, messages []Message, _ []Definition, _ Emit) (Message, error) {
				if messages[len(messages)-1].Text() != name {
					t.Errorf("foreign agent prompt: %+v", messages)
				}
				return Text("assistant", name), nil
			}}}
			if text, err := loop.Run(context.Background(), name); err != nil || text != name {
				t.Errorf("run failed: %q %v", text, err)
			}
			loop.Checkpoint.LastCompaction.Quotes[0].Text = name
			loop.Checkpoint.LastCompaction.Usage.InputTokens = 99
		}()
	}
	wg.Wait()
	if seed[0].Text() != "shared immutable origin" || !reflect.DeepEqual(seed[1:cap(seed)], make([]Message, cap(seed)-1)) || checkpoint.LastSequence != 1 || checkpoint.LastCompaction.Quotes[0].Text != "original quote" || checkpoint.LastCompaction.Usage.InputTokens != 3 {
		t.Fatalf("seed mutated by a child session: %+v %+v", seed[:cap(seed)], checkpoint)
	}
}

func TestCompactionObserverCannotRewriteRecoveryView(t *testing.T) {
	record := &CompactionRecord{CompactionCheckpoint: CompactionCheckpoint{Quotes: []SummaryQuote{{Text: "exact evidence"}}, Usage: &Usage{InputTokens: 4}}, View: []Message{Text("user", "private request view")}}
	loop := &Loop{Emit: func(event Event) {
		event.Compaction.View[0].Content[0].Text = "observer scratch"
		event.Compaction.Quotes[0].Text = "rewritten quote"
		event.Compaction.Usage.InputTokens = 99
	}}
	loop.emit(Event{Type: "context_compaction_prepared", Compaction: record})
	if record.View[0].Text() != "private request view" || record.Quotes[0].Text != "exact evidence" || record.Usage.InputTokens != 4 {
		t.Fatalf("event observer rewrote recovery data: %+v", record)
	}
}

func TestPublicMutatorsOwnSharedSeedBeforeRun(t *testing.T) {
	for _, repair := range []bool{false, true} {
		name := "append"
		if repair {
			name = "repair_then_append"
		}
		t.Run(name, func(t *testing.T) {
			seed := make([]Message, 1, 16)
			seed[0] = Text("user", "shared immutable task")
			seed[0].Sequence = 1
			if repair {
				seed = append(seed, Message{Role: "assistant", Sequence: 2, Content: []Block{{Type: "tool_use", ID: "pending", Name: "inspect", Input: json.RawMessage(`{}`)}}})
			}
			checkpoint := &ContextCheckpoint{Version: ContextCheckpointVersion, LastSequence: uint64(len(seed))}
			loops := []*Loop{{History: seed, Checkpoint: checkpoint}, {History: seed, Checkpoint: checkpoint}}
			for i, loop := range loops {
				if repair {
					if err := loop.RepairHistory(); err != nil {
						t.Fatal(err)
					}
					result := loop.History[len(seed)].Content[0]
					if result.Type != "tool_result" || result.ToolUseID != "pending" || !result.IsError {
						t.Fatalf("interrupted call was not settled: %+v", result)
					}
				}
				if err := loop.AppendInstruction([]string{"private-a", "private-b"}[i]); err != nil {
					t.Fatal(err)
				}
			}
			for i, loop := range loops {
				want := []string{"private-a", "private-b"}[i]
				last := loop.History[len(loop.History)-1]
				if last.Text() != want || last.Sequence != uint64(len(loop.History)) {
					t.Fatalf("pre-Run mutator shared instructions or sequence state: %+v", loop.History)
				}
				ownedHistory, ownedCheckpoint := &loop.History[0], loop.Checkpoint
				loop.Provider = observedProvider{generate: func(_ context.Context, history []Message, _ []Definition, _ Emit) (Message, error) {
					if history[len(history)-1].Text() != want {
						t.Fatalf("Run started with another agent's instruction: %+v", history)
					}
					return Text("assistant", "done"), nil
				}}
				// No second full seed copy is needed at Run after public mutation.
				// Run's ordinary append may grow History, so inspect before it.
				loop.BeforeRequest = func(ctx context.Context, current *Loop) (context.Context, error) {
					if &current.History[0] != ownedHistory || current.Checkpoint != ownedCheckpoint {
						t.Fatal("Run copied already-owned state again")
					}
					return ctx, nil
				}
				if _, err := loop.Run(context.Background(), ""); err != nil {
					t.Fatal(err)
				}
			}
			if checkpoint.LastSequence != uint64(len(seed)) || !reflect.DeepEqual(seed[len(seed):cap(seed)], make([]Message, cap(seed)-len(seed))) {
				t.Fatalf("public mutator rewrote shared seed: %+v checkpoint=%+v", seed[:cap(seed)], checkpoint)
			}
		})
	}
}
