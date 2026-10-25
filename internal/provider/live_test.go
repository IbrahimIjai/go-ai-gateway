package provider

import (
	"context"
	"os"
	"testing"
	"time"

	"example.com/minirouter/internal/core"
)

// TestLiveToolCall runs one forced tool call against each provider whose key
// is set. It costs real money, so it runs only with MINIROUTER_LIVE_TESTS=1.
func TestLiveToolCall(t *testing.T) {
	if os.Getenv("MINIROUTER_LIVE_TESTS") != "1" {
		t.Skip("set MINIROUTER_LIVE_TESTS=1 and provider keys to call live APIs")
	}
	model := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}
	providers := map[string]struct {
		p     Provider
		model string
	}{}
	if k := os.Getenv("OPENAI_API_KEY"); k != "" {
		providers["openai"] = struct {
			p     Provider
			model string
		}{&OpenAI{Key: k}, model("OPENAI_MODEL", "gpt-4.1-mini")}
	}
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		providers["anthropic"] = struct {
			p     Provider
			model string
		}{&Anthropic{Key: k}, model("ANTHROPIC_MODEL", "claude-haiku-4-5")}
	}
	if k := os.Getenv("GEMINI_API_KEY"); k != "" {
		providers["gemini"] = struct {
			p     Provider
			model string
		}{&Gemini{Key: k}, model("GEMINI_MODEL", "gemini-2.5-flash")}
	}
	for name, tc := range providers {
		t.Run(name, func(t *testing.T) {
			req := toolConversation()
			req.Messages = req.Messages[:2]
			req.ToolChoice = &core.ToolChoice{Mode: "function", Name: "get_weather"}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			out, err := tc.p.Complete(ctx, tc.model, req)
			if err != nil || len(out.ToolCalls) == 0 || out.ToolCalls[0].Name != "get_weather" || out.Usage == nil {
				t.Fatalf("%+v %v", out, err)
			}
			if _, err := jsonObject(name, out.ToolCalls[0].Arguments); err != nil {
				t.Fatalf("arguments are not a JSON object: %q", out.ToolCalls[0].Arguments)
			}
		})
	}
}
