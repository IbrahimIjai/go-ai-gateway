package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"example.com/minirouter/internal/core"
)

type Anthropic struct {
	Key, BaseURL string
	Client       *http.Client
	// StreamIdleTimeout cuts a stream off after this long without upstream
	// bytes; zero means 120 s.
	StreamIdleTimeout time.Duration
}

func (p *Anthropic) Name() string { return "anthropic" }
func (p *Anthropic) base() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return "https://api.anthropic.com"
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // a string or content blocks
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

// anthropicToolID maps any tool call ID onto Anthropic's [a-zA-Z0-9_-]+
// alphabet. It is deterministic, so a tool_use and its tool_result still pair
// up, and the hash keeps distinct IDs distinct (as Bifrost's
// SanitizeAnthropicToolUseID does).
func anthropicToolID(id string) string {
	safe := func(r rune) bool {
		return r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
	}
	if id != "" && strings.IndexFunc(id, func(r rune) bool { return !safe(r) }) < 0 {
		return id
	}
	kept := strings.Trim(strings.Map(func(r rune) rune {
		if safe(r) {
			return r
		}
		return '_'
	}, id), "_")
	if len(kept) > 32 {
		kept = kept[:32]
	}
	sum := sha256.Sum256([]byte(id))
	return kept + "_" + hex.EncodeToString(sum[:8])
}

func anthropicContent(m core.Message) any {
	if len(m.Content) == 1 {
		return m.Content[0].Text
	}
	blocks := make([]anthropicBlock, 0, len(m.Content))
	for _, p := range m.Content {
		blocks = append(blocks, anthropicBlock{Type: "text", Text: p.Text})
	}
	return blocks
}

func (p *Anthropic) payload(model string, req core.ChatRequest, stream bool) (any, error) {
	if f := req.ResponseFormat; f != nil {
		return nil, &UnsupportedError{Provider: "anthropic", Feature: "response_format " + f.Type}
	}
	var system []string
	var messages []anthropicMessage
	for i := 0; i < len(req.Messages); i++ {
		m := req.Messages[i]
		switch m.Role {
		case core.RoleSystem, core.RoleDeveloper:
			system = append(system, m.Text())
		case core.RoleTool:
			// Consecutive tool results travel together in one user turn.
			var results []anthropicBlock
			for ; i < len(req.Messages) && req.Messages[i].Role == core.RoleTool; i++ {
				t := req.Messages[i]
				results = append(results, anthropicBlock{Type: "tool_result", ToolUseID: anthropicToolID(t.ToolCallID), Content: t.Text()})
			}
			i--
			messages = append(messages, anthropicMessage{Role: "user", Content: results})
		case core.RoleAssistant:
			var blocks []anthropicBlock
			for _, part := range m.Content {
				if part.Text != "" { // Anthropic rejects empty text blocks
					blocks = append(blocks, anthropicBlock{Type: "text", Text: part.Text})
				}
			}
			for _, c := range m.ToolCalls {
				input, err := jsonObject("anthropic", c.Arguments)
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, anthropicBlock{Type: "tool_use", ID: anthropicToolID(c.ID), Name: c.Name, Input: input})
			}
			if len(blocks) == 0 {
				messages = append(messages, anthropicMessage{Role: "assistant", Content: ""})
				continue
			}
			messages = append(messages, anthropicMessage{Role: "assistant", Content: blocks})
		default:
			messages = append(messages, anthropicMessage{Role: "user", Content: anthropicContent(m)})
		}
	}
	v := map[string]any{"model": model, "messages": messages, "max_tokens": req.MaxOutputTokens, "stream": stream}
	if len(system) > 0 {
		v["system"] = strings.Join(system, "\n\n")
	}
	if req.Temperature != nil {
		v["temperature"] = *req.Temperature
	}
	if len(req.Stop) > 0 {
		v["stop_sequences"] = req.Stop
	}
	if len(req.Tools) > 0 {
		tools := make([]any, len(req.Tools))
		for i, t := range req.Tools {
			if t.Strict {
				return nil, &UnsupportedError{Provider: "anthropic", Feature: "strict tool schemas"}
			}
			schema := t.Parameters
			if schema == nil {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tool := map[string]any{"name": t.Name, "input_schema": schema}
			if t.Description != "" {
				tool["description"] = t.Description
			}
			tools[i] = tool
		}
		v["tools"] = tools
	}
	choice := map[string]any{}
	if c := req.ToolChoice; c != nil {
		switch c.Mode {
		case "required":
			choice["type"] = "any"
		case "function":
			choice["type"], choice["name"] = "tool", c.Name
		default: // auto, none
			choice["type"] = c.Mode
		}
	}
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		if choice["type"] == nil {
			choice["type"] = "auto"
		}
		choice["disable_parallel_tool_use"] = true
	}
	if len(choice) > 0 {
		v["tool_choice"] = choice
	}
	return v, nil
}
func (p *Anthropic) send(ctx context.Context, model string, req core.ChatRequest, stream bool) (*http.Response, error) {
	body, err := p.payload(model, req, stream)
	if err != nil {
		return nil, err
	}
	return postJSON(ctx, defaultHTTP(p.Client), endpoint(p.base(), "/v1/messages"), body, map[string]string{"x-api-key": p.Key, "anthropic-version": "2023-06-01"})
}

// Anthropic returns cache-read and cache-created tokens separately from
// ordinary input_tokens. Normalize InputTokens as their sum.
type anthropicUsage struct {
	Input  int64 `json:"input_tokens"`
	Output int64 `json:"output_tokens"`
	Read   int64 `json:"cache_read_input_tokens"`
	Write  int64 `json:"cache_creation_input_tokens"`
}

func (u anthropicUsage) core(raw json.RawMessage) *core.Usage {
	v := &core.Usage{InputTokens: u.Input + u.Read + u.Write, OutputTokens: u.Output, CachedInput: u.Read, CacheWrite: u.Write, Raw: raw}
	v.Normalize()
	return v
}
func anthroFinish(s string) string {
	switch s {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default: // end_turn, stop_sequence, pause_turn
		return "stop"
	}
}

type anthropicResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      json.RawMessage `json:"usage"`
}

func (p *Anthropic) Complete(ctx context.Context, model string, req core.ChatRequest) (core.Completion, error) {
	resp, err := p.send(ctx, model, req, false)
	if err != nil {
		return core.Completion{}, err
	}
	raw, err := readJSON[anthropicResponse](resp)
	if err != nil {
		return core.Completion{}, err
	}
	out := core.Completion{FinishReason: anthroFinish(raw.StopReason)}
	var text strings.Builder
	for _, c := range raw.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, core.ToolCall{ID: c.ID, Name: c.Name, Arguments: argumentsText(c.Input)})
		default:
			return core.Completion{}, fmt.Errorf("unsupported anthropic response block: %s", c.Type)
		}
	}
	out.Text = text.String()
	if len(raw.Usage) > 0 && string(raw.Usage) != "null" {
		var u anthropicUsage
		if err := json.Unmarshal(raw.Usage, &u); err != nil {
			return core.Completion{}, fmt.Errorf("invalid anthropic usage: %w", err)
		}
		out.Usage = u.core(raw.Usage)
	}
	return out, nil
}
func (p *Anthropic) Stream(ctx context.Context, model string, req core.ChatRequest) (Stream, error) {
	body, err := openStream(ctx, p.StreamIdleTimeout, func(ctx context.Context) (*http.Response, error) {
		return p.send(ctx, model, req, true)
	})
	if err != nil {
		return nil, err
	}
	return &anthroStream{body: body, events: newEventReader(body), tools: map[int]*anthroTool{}}, nil
}

type anthroStream struct {
	body     io.ReadCloser
	events   *eventReader
	usage    anthropicUsage
	hasUsage bool
	finished bool
	tools    map[int]*anthroTool // content block index → tool call
	next     int                 // index the next tool call gets in the response
}

type anthroTool struct {
	index   int
	gotArgs bool
}

func (s *anthroStream) Close() error { return s.body.Close() }
func (s *anthroStream) Recv() (core.Chunk, error) {
	if s.finished {
		return core.Chunk{}, io.EOF
	}
	for {
		e, err := s.events.Next()
		if err == io.EOF {
			return core.Chunk{}, io.ErrUnexpectedEOF // message_stop is the only end marker
		}
		if err != nil {
			return core.Chunk{}, err
		}
		switch e.Name {
		case "message_start":
			var v struct {
				Message struct {
					Usage anthropicUsage `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(e.Data), &v); err != nil {
				return core.Chunk{}, err
			}
			s.usage = v.Message.Usage
			s.hasUsage = true
		case "content_block_start":
			var v struct {
				Index int `json:"index"`
				Block struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(e.Data), &v); err != nil {
				return core.Chunk{}, err
			}
			switch v.Block.Type {
			case "text":
			case "tool_use":
				t := &anthroTool{index: s.next}
				s.next++
				s.tools[v.Index] = t
				return core.Chunk{ToolCalls: []core.ToolCallDelta{{Index: t.index, ID: v.Block.ID, Name: v.Block.Name}}}, nil
			default:
				return core.Chunk{}, fmt.Errorf("unsupported anthropic streaming block: %s", v.Block.Type)
			}
		case "content_block_delta":
			var v struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(e.Data), &v); err != nil {
				return core.Chunk{}, err
			}
			switch v.Delta.Type {
			case "text_delta":
				if v.Delta.Text != "" {
					return core.Chunk{Text: v.Delta.Text}, nil
				}
			case "input_json_delta":
				t := s.tools[v.Index]
				if t == nil {
					return core.Chunk{}, fmt.Errorf("anthropic sent tool input for unknown block %d", v.Index)
				}
				if v.Delta.PartialJSON != "" {
					t.gotArgs = true
					return core.Chunk{ToolCalls: []core.ToolCallDelta{{Index: t.index, Arguments: v.Delta.PartialJSON}}}, nil
				}
			default:
				return core.Chunk{}, fmt.Errorf("unsupported anthropic streaming delta: %s", v.Delta.Type)
			}
		case "content_block_stop":
			var v struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal([]byte(e.Data), &v); err != nil {
				return core.Chunk{}, err
			}
			// A call without input streams no JSON at all; OpenAI clients
			// expect "{}" rather than empty arguments.
			if t := s.tools[v.Index]; t != nil && !t.gotArgs {
				t.gotArgs = true
				return core.Chunk{ToolCalls: []core.ToolCallDelta{{Index: t.index, Arguments: "{}"}}}, nil
			}
		case "message_delta":
			var v struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				// Counts here are cumulative; absent fields keep earlier values.
				Usage struct {
					Input  *int64 `json:"input_tokens"`
					Output *int64 `json:"output_tokens"`
					Read   *int64 `json:"cache_read_input_tokens"`
					Write  *int64 `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(e.Data), &v); err != nil {
				return core.Chunk{}, err
			}
			for _, f := range []struct{ dst, src *int64 }{{&s.usage.Input, v.Usage.Input}, {&s.usage.Output, v.Usage.Output}, {&s.usage.Read, v.Usage.Read}, {&s.usage.Write, v.Usage.Write}} {
				if f.src != nil {
					*f.dst = *f.src
				}
			}
			c := core.Chunk{FinishReason: anthroFinish(v.Delta.StopReason)}
			if s.hasUsage {
				raw, _ := json.Marshal(s.usage)
				c.Usage = s.usage.core(raw)
			}
			return c, nil
		case "message_stop":
			s.finished = true
			return core.Chunk{}, io.EOF
		case "error":
			return core.Chunk{}, newStreamError([]byte(e.Data))
		}
	}
}
