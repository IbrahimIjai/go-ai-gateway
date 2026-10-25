package provider

import (
	"context"
	"strings"
	"unicode/utf8"

	"example.com/minirouter/internal/core"
)

type Demo struct{}

func (Demo) Name() string { return "demo" }
func demoCompletion(req core.ChatRequest) core.Completion {
	prompt := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			prompt = m.Content
		}
	}
	text := "[DEMO ONLY — not an LLM] You said: " + prompt
	// Toy counts for demo only. Real providers use authoritative metadata.
	var chars int
	for _, m := range req.Messages {
		chars += utf8.RuneCountInString(m.Content)
	}
	usage := &core.Usage{InputTokens: int64(chars/4 + 1), OutputTokens: int64(utf8.RuneCountInString(text)/4 + 1)}
	usage.Normalize()
	return core.Completion{Text: text, FinishReason: "stop", Usage: usage}
}
func (Demo) Complete(_ context.Context, _ string, req core.ChatRequest) (core.Completion, error) {
	return demoCompletion(req), nil
}
func (Demo) Stream(_ context.Context, _ string, req core.ChatRequest) (Stream, error) {
	resp := demoCompletion(req)
	words := strings.SplitAfter(resp.Text, " ")
	chunks := make([]core.Chunk, 0, len(words)+1)
	for _, part := range words {
		if part != "" {
			chunks = append(chunks, core.Chunk{Text: part})
		}
	}
	chunks = append(chunks, core.Chunk{FinishReason: "stop", Usage: resp.Usage})
	return &sliceStream{chunks: chunks}, nil
}
