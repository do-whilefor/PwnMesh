package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
)

func TestStreamTextFragmentsSurviveCompletionAndInterruption(t *testing.T) {
	const prefix = `data: {"type":"message_start"}

data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"思-","signature":"sig-"}}

data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"pre-"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"考"}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hello "}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"first"}}

data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":"initial only"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"中"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"-last"}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"世界"}}

`
	const ending = "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, tc := range []struct {
		name, suffix string
		readError    error
		cancel       bool
	}{
		{name: "complete", suffix: ending},
		{name: "EOF"},
		{name: "read error", readError: io.ErrUnexpectedEOF},
		{name: "invalid event", suffix: "data: {\n\n"},
		{name: "endpoint error", suffix: "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n"},
		{name: "cancelled", suffix: ending, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var reader io.Reader = strings.NewReader(prefix + tc.suffix)
			if tc.readError != nil {
				reader = io.MultiReader(reader, failingReader{tc.readError})
			}
			var emitted strings.Builder
			message, err := consumeSSE(ctx, reader, func(event agent.Event) {
				if event.Type == "text_delta" {
					emitted.WriteString(event.Text)
					if tc.cancel && event.Text == "世界" {
						cancel()
					}
				}
			})
			if (err == nil) != (tc.name == "complete") || tc.readError != nil && !errors.Is(err, tc.readError) || tc.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("unexpected stream error: %v", err)
			}
			if len(message.Content) != 3 || message.Content[0].Thinking != "思-考中" || message.Content[0].Signature != "sig-first-last" || message.Content[1].Text != "pre-hello 世界" || message.Content[2].Text != "initial only" {
				t.Fatalf("lost initial or fragmented block fields: %+v", message.Content)
			}
			if emitted.String() != "hello 世界" {
				t.Fatalf("changed emitted deltas: %q", emitted.String())
			}
		})
	}
}

func BenchmarkConsumeSSETextFragments(b *testing.B) {
	for _, field := range []string{"text", "thinking", "signature"} {
		b.Run(field, func(b *testing.B) {
			const chunks = 2048
			fragment := strings.Repeat("x", 128)
			kind := "thinking"
			if field == "text" {
				kind = "text"
			}
			var stream strings.Builder
			stream.WriteString("data: {\"type\":\"message_start\"}\n\n")
			fmt.Fprintf(&stream, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":%q}}\n\n", kind)
			for n := 0; n < chunks; n++ {
				fmt.Fprintf(&stream, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":%q,%q:%q}}\n\n", field+"_delta", field, fragment)
			}
			stream.WriteString("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
			raw := stream.String()
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				message, err := consumeSSE(context.Background(), strings.NewReader(raw), nil)
				if err != nil || len(message.Content) != 1 {
					b.Fatalf("stream failed: %v", err)
				}
				block := message.Content[0]
				if len(block.Text)+len(block.Thinking)+len(block.Signature) != chunks*len(fragment) {
					b.Fatal("stream content was truncated")
				}
			}
		})
	}
}
