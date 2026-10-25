package provider

import (
	"context"
	"strings"
	"unicode/utf8"

	"example.com/minirouter/internal/core"
)

// Demo is a deterministic offline adapter for contract tests. It is not an
// LLM. Given tools, it calls one, then echoes the tool's result, so tool
// loops can be exercised without credentials.
type Demo struct{}

func (Demo) Name() string { return "demo" }
func demoCompletion(req core.ChatRequest) core.Completion {
	var out core.Completion
	last := req.Messages[len(req.Messages)-1]
	switch {
	case last.Role == core.RoleTool:
		out = core.Completion{Text: "[DEMO ONLY — not an LLM] Tool result: " + last.Text(), FinishReason: "stop"}
	case len(req.Tools) > 0 && (req.ToolChoice == nil || req.ToolChoice.Mode != "none"):
		name := req.Tools[0].Name
		if req.ToolChoice != nil && req.ToolChoice.Mode == "function" {
			name = req.ToolChoice.Name
		}
		out = core.Completion{ToolCalls: []core.ToolCall{{ID: "call_demo_1", Name: name, Arguments: "{}"}}, FinishReason: "tool_calls"}
	default:
		prompt := ""
		for _, m := range req.Messages {
			if m.Role == core.RoleUser {
				prompt = m.Text()
			}
		}
		out = core.Completion{Text: "[DEMO ONLY — not an LLM] You said: " + prompt, FinishReason: "stop"}
	}
	// Toy counts for demo only. Real providers use authoritative metadata.
	var chars int
	for _, m := range req.Messages {
		chars += utf8.RuneCountInString(m.Text())
	}
	out.Usage = &core.Usage{InputTokens: int64(chars/4 + 1), OutputTokens: int64(utf8.RuneCountInString(out.Text)/4 + 1)}
	out.Usage.Normalize()
	return out
}
func (Demo) Complete(_ context.Context, _ string, req core.ChatRequest) (core.Completion, error) {
	return demoCompletion(req), nil
}
func (Demo) Stream(_ context.Context, _ string, req core.ChatRequest) (Stream, error) {
	resp := demoCompletion(req)
	var chunks []core.Chunk
	for _, part := range strings.SplitAfter(resp.Text, " ") {
		if part != "" {
			chunks = append(chunks, core.Chunk{Text: part})
		}
	}
	for i, c := range resp.ToolCalls {
		chunks = append(chunks, core.Chunk{ToolCalls: []core.ToolCallDelta{{Index: i, ID: c.ID, Name: c.Name, Arguments: c.Arguments}}})
	}
	chunks = append(chunks, core.Chunk{FinishReason: resp.FinishReason, Usage: resp.Usage})
	return &sliceStream{chunks: chunks}, nil
}
