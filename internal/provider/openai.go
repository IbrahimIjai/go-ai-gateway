package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"example.com/minirouter/internal/core"
)

// OpenAI speaks the Chat Completions protocol, to OpenAI itself or to any
// OpenAI-compatible host (Moonshot, Together, Fireworks, ...).
type OpenAI struct {
	Key     string
	BaseURL string // including the version path; default https://api.openai.com/v1
	Client  *http.Client
	// MaxTokensField names the output limit parameter; empty means
	// max_completion_tokens. Many compatible hosts accept only max_tokens.
	MaxTokensField string
	// DeveloperAsSystem sends developer messages as system ones, for hosts
	// that predate the developer role.
	DeveloperAsSystem bool
	// StreamIdleTimeout cuts a stream off after this long without upstream
	// bytes; zero means 120 s.
	StreamIdleTimeout time.Duration
}

func (p *OpenAI) Name() string { return "openai" }
func (p *OpenAI) base() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return "https://api.openai.com/v1"
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"` // string, text parts, or null beside tool calls
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	Index    *int   `json:"index,omitempty"` // stream deltas only
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func openAIContent(m core.Message) any {
	switch {
	case len(m.Content) == 0 && m.Role == core.RoleAssistant:
		return nil
	case len(m.Content) == 0:
		return ""
	case len(m.Content) == 1:
		return m.Content[0].Text
	}
	parts := make([]map[string]string, len(m.Content))
	for i, p := range m.Content {
		parts[i] = map[string]string{"type": "text", "text": p.Text}
	}
	return parts
}

func (p *OpenAI) payload(model string, req core.ChatRequest, stream bool) any {
	messages := make([]openAIMessage, len(req.Messages))
	for i, m := range req.Messages {
		role := string(m.Role)
		if m.Role == core.RoleDeveloper && p.DeveloperAsSystem {
			role = string(core.RoleSystem)
		}
		messages[i] = openAIMessage{Role: role, Content: openAIContent(m), ToolCallID: m.ToolCallID}
		for _, c := range m.ToolCalls {
			var call openAIToolCall
			call.ID, call.Type = c.ID, "function"
			call.Function.Name, call.Function.Arguments = c.Name, c.Arguments
			messages[i].ToolCalls = append(messages[i].ToolCalls, call)
		}
	}
	limit := p.MaxTokensField
	if limit == "" {
		limit = "max_completion_tokens"
	}
	v := map[string]any{"model": model, "messages": messages, limit: req.MaxOutputTokens, "stream": stream}
	if req.Temperature != nil {
		v["temperature"] = *req.Temperature
	}
	if len(req.Stop) > 0 {
		v["stop"] = req.Stop
	}
	if len(req.Tools) > 0 {
		tools := make([]any, len(req.Tools))
		for i, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if t.Parameters != nil {
				fn["parameters"] = t.Parameters
			}
			if t.Strict {
				fn["strict"] = true
			}
			tools[i] = map[string]any{"type": "function", "function": fn}
		}
		v["tools"] = tools
	}
	if c := req.ToolChoice; c != nil {
		if c.Mode == "function" {
			v["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": c.Name}}
		} else {
			v["tool_choice"] = c.Mode
		}
	}
	if req.ParallelToolCalls != nil {
		v["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if f := req.ResponseFormat; f != nil {
		if f.Type == "json_schema" {
			schema := map[string]any{"name": f.Name, "schema": f.Schema}
			if f.Strict {
				schema["strict"] = true
			}
			v["response_format"] = map[string]any{"type": "json_schema", "json_schema": schema}
		} else {
			v["response_format"] = map[string]string{"type": f.Type}
		}
	}
	if stream {
		v["stream_options"] = map[string]bool{"include_usage": true}
	}
	return v
}
func (p *OpenAI) send(ctx context.Context, model string, req core.ChatRequest, stream bool) (*http.Response, error) {
	return postJSON(ctx, defaultHTTP(p.Client), endpoint(p.base(), "/chat/completions"), p.payload(model, req, stream), map[string]string{"Authorization": "Bearer " + p.Key})
}

type openAIUsage struct {
	Input         int64 `json:"prompt_tokens"`
	Output        int64 `json:"completion_tokens"`
	PromptDetails struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	OutputDetails struct {
		Reasoning int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// openAIUsageFrom decodes a usage object and keeps its original bytes. An
// absent usage object stays nil: unknown, not zero.
func openAIUsageFrom(raw json.RawMessage) (*core.Usage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var u openAIUsage
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, fmt.Errorf("invalid openai usage: %w", err)
	}
	v := &core.Usage{InputTokens: u.Input, OutputTokens: u.Output, CachedInput: u.PromptDetails.Cached, ReasoningTokens: u.OutputDetails.Reasoning, Raw: raw}
	v.Normalize()
	return v, nil
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content   *string          `json:"content"`
			Refusal   *string          `json:"refusal"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

func (p *OpenAI) Complete(ctx context.Context, model string, req core.ChatRequest) (core.Completion, error) {
	resp, err := p.send(ctx, model, req, false)
	if err != nil {
		return core.Completion{}, err
	}
	raw, err := readJSON[openAIResponse](resp)
	if err != nil {
		return core.Completion{}, err
	}
	if len(raw.Choices) == 0 {
		return core.Completion{}, fmt.Errorf("openai returned no choices")
	}
	msg := raw.Choices[0].Message
	out := core.Completion{FinishReason: raw.Choices[0].FinishReason}
	if msg.Content != nil {
		out.Text = *msg.Content
	}
	if msg.Refusal != nil {
		out.Refusal = *msg.Refusal
	}
	for _, c := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, core.ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	if out.Usage, err = openAIUsageFrom(raw.Usage); err != nil {
		return core.Completion{}, err
	}
	return out, nil
}
func (p *OpenAI) Stream(ctx context.Context, model string, req core.ChatRequest) (Stream, error) {
	body, err := openStream(ctx, p.StreamIdleTimeout, func(ctx context.Context) (*http.Response, error) {
		return p.send(ctx, model, req, true)
	})
	if err != nil {
		return nil, err
	}
	return &openAIStream{body: body, events: newEventReader(body)}, nil
}

type openAIStream struct {
	body   io.ReadCloser
	events *eventReader
	done   bool
	// finished records a finish_reason. Some OpenAI-compatible hosts end with
	// it instead of [DONE], so EOF after it is a clean end; EOF before is a cut.
	finished bool
}

func (s *openAIStream) Close() error { return s.body.Close() }
func (s *openAIStream) Recv() (core.Chunk, error) {
	if s.done {
		return core.Chunk{}, io.EOF
	}
	for {
		e, err := s.events.Next()
		if err == io.EOF {
			if s.finished {
				s.done = true
				return core.Chunk{}, io.EOF
			}
			return core.Chunk{}, io.ErrUnexpectedEOF
		}
		if err != nil {
			return core.Chunk{}, err
		}
		if e.Data == "[DONE]" {
			s.done = true
			return core.Chunk{}, io.EOF
		}
		if strings.TrimSpace(e.Data) == "" {
			continue
		}
		var v struct {
			Choices []struct {
				Delta struct {
					Content   string           `json:"content"`
					Refusal   string           `json:"refusal"`
					ToolCalls []openAIToolCall `json:"tool_calls"`
				} `json:"delta"`
				Finish string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if err = json.Unmarshal([]byte(e.Data), &v); err != nil {
			return core.Chunk{}, fmt.Errorf("invalid openai SSE: %w", err)
		}
		if len(v.Error) > 0 && string(v.Error) != "null" {
			return core.Chunk{}, newStreamError([]byte(e.Data))
		}
		var chunk core.Chunk
		if len(v.Choices) > 0 {
			d := v.Choices[0].Delta
			chunk.Text, chunk.Refusal, chunk.FinishReason = d.Content, d.Refusal, v.Choices[0].Finish
			for _, c := range d.ToolCalls {
				index := 0
				if c.Index != nil {
					index = *c.Index
				}
				chunk.ToolCalls = append(chunk.ToolCalls, core.ToolCallDelta{Index: index, ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
			}
		}
		if chunk.Usage, err = openAIUsageFrom(v.Usage); err != nil {
			return core.Chunk{}, err
		}
		if chunk.FinishReason != "" {
			s.finished = true
		}
		if chunk.HasContent() || chunk.Usage != nil {
			return chunk, nil
		}
	}
}
