package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"example.com/minirouter/internal/core"
)

// chatRequest is the OpenAI Chat Completions subset MiniRouter accepts.
// Unknown fields are rejected, so nothing a client asks for is dropped.
type chatRequest struct {
	Model             string          `json:"model"`
	Messages          []chatMessage   `json:"messages"`
	Tools             []chatTool      `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	ResponseFormat    *struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict"`
		} `json:"json_schema"`
	} `json:"response_format"`
	Stop          json.RawMessage `json:"stop"`
	Stream        bool            `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Temperature         *float64 `json:"temperature"`
	MaxCompletionTokens *int     `json:"max_completion_tokens"`
	MaxTokens           *int     `json:"max_tokens"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // a string, text parts, or null beside tool calls
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      *bool           `json:"strict"`
	} `json:"function"`
}

const (
	maxMessages     = 1000
	maxMessageBytes = 1 << 20
	maxTools        = 128
)

// strictDecode decodes one JSON value, rejecting unknown fields.
func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

// validName matches OpenAI's rule for function names.
func validName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

func isJSONObject(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil && obj != nil
}

// parseContent reads message content: a string or an array of text parts.
func parseContent(raw json.RawMessage) ([]core.Part, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []core.Part{{Text: s}}, nil
	}
	var parts []struct {
		Type     string          `json:"type"`
		Text     *string         `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if err := strictDecode(raw, &parts); err != nil {
		return nil, errors.New("content must be a string or an array of content parts")
	}
	out := make([]core.Part, 0, len(parts))
	for _, p := range parts {
		switch {
		case p.Type == "image_url":
			return nil, errors.New("image input is not supported yet")
		case p.Type != "text" || p.Text == nil:
			return nil, fmt.Errorf("unsupported content part type %q", p.Type)
		}
		out = append(out, core.Part{Text: *p.Text})
	}
	return out, nil
}

// toCore validates the request and converts it to MiniRouter's own terms.
func (c chatRequest) toCore() (core.ChatRequest, error) {
	req := core.ChatRequest{Model: c.Model, Stream: c.Stream, Temperature: c.Temperature}
	if c.Model == "" {
		return req, errors.New("model is required")
	}
	if len(c.Messages) == 0 || len(c.Messages) > maxMessages {
		return req, fmt.Errorf("messages must contain 1–%d entries", maxMessages)
	}
	if c.MaxCompletionTokens != nil && c.MaxTokens != nil {
		return req, errors.New("use only one of max_tokens and max_completion_tokens")
	}
	req.MaxOutputTokens = 512
	if c.MaxCompletionTokens != nil {
		req.MaxOutputTokens = *c.MaxCompletionTokens
	} else if c.MaxTokens != nil {
		req.MaxOutputTokens = *c.MaxTokens
	}
	if req.MaxOutputTokens < 1 || req.MaxOutputTokens > 8192 {
		return req, errors.New("max output tokens must be between 1 and 8192")
	}
	if c.Temperature != nil && (*c.Temperature < 0 || *c.Temperature > 2) {
		return req, errors.New("temperature must be between 0 and 2")
	}
	if c.StreamOptions != nil {
		if !c.Stream {
			return req, errors.New("stream_options requires stream=true")
		}
		req.IncludeUsage = c.StreamOptions.IncludeUsage
	}
	if !isNull(c.Stop) {
		var one string
		if json.Unmarshal(c.Stop, &one) == nil {
			req.Stop = []string{one}
		} else if json.Unmarshal(c.Stop, &req.Stop) != nil {
			return req, errors.New("stop must be a string or an array of strings")
		}
		if len(req.Stop) > 4 {
			return req, errors.New("stop accepts at most 4 sequences")
		}
		for _, s := range req.Stop {
			if s == "" {
				return req, errors.New("stop sequences must not be empty")
			}
		}
	}
	if err := c.convertTools(&req); err != nil {
		return req, err
	}
	if f := c.ResponseFormat; f != nil {
		switch f.Type {
		case "text":
		case "json_object":
			req.ResponseFormat = &core.ResponseFormat{Type: f.Type}
		case "json_schema":
			s := f.JSONSchema
			if s == nil || !validName(s.Name) || !isJSONObject(s.Schema) {
				return req, errors.New("response_format json_schema needs a valid name and a schema object")
			}
			req.ResponseFormat = &core.ResponseFormat{Type: f.Type, Name: s.Name, Schema: s.Schema, Strict: s.Strict != nil && *s.Strict}
		default:
			return req, fmt.Errorf("unsupported response_format type %q", f.Type)
		}
	}
	return req, c.convertMessages(&req)
}

func (c chatRequest) convertTools(req *core.ChatRequest) error {
	if len(c.Tools) > maxTools {
		return fmt.Errorf("at most %d tools are accepted", maxTools)
	}
	names := map[string]bool{}
	for _, t := range c.Tools {
		if t.Type != "function" {
			return fmt.Errorf("unsupported tool type %q", t.Type)
		}
		f := t.Function
		if !validName(f.Name) || names[f.Name] {
			return fmt.Errorf("tool name %q must be unique and match [a-zA-Z0-9_-]{1,64}", f.Name)
		}
		names[f.Name] = true
		tool := core.Tool{Name: f.Name, Description: f.Description, Strict: f.Strict != nil && *f.Strict}
		if !isNull(f.Parameters) {
			if !isJSONObject(f.Parameters) {
				return fmt.Errorf("tool %q: parameters must be a JSON Schema object", f.Name)
			}
			tool.Parameters = f.Parameters
		}
		req.Tools = append(req.Tools, tool)
	}
	if !isNull(c.ToolChoice) {
		if len(c.Tools) == 0 {
			return errors.New("tool_choice requires tools")
		}
		var mode string
		if json.Unmarshal(c.ToolChoice, &mode) == nil {
			if mode != "auto" && mode != "none" && mode != "required" {
				return fmt.Errorf("unsupported tool_choice %q", mode)
			}
			req.ToolChoice = &core.ToolChoice{Mode: mode}
		} else {
			var forced struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if strictDecode(c.ToolChoice, &forced) != nil || forced.Type != "function" || !names[forced.Function.Name] {
				return errors.New("tool_choice must be auto, none, required, or name one of the tools")
			}
			req.ToolChoice = &core.ToolChoice{Mode: "function", Name: forced.Function.Name}
		}
	}
	if c.ParallelToolCalls != nil {
		if len(c.Tools) == 0 {
			return errors.New("parallel_tool_calls requires tools")
		}
		req.ParallelToolCalls = c.ParallelToolCalls
	}
	return nil
}

func (c chatRequest) convertMessages(req *core.ChatRequest) error {
	calls := map[string]bool{} // tool call IDs seen so far
	hasUser := false
	for i, m := range c.Messages {
		msg := core.Message{Role: core.Role(m.Role)}
		switch msg.Role {
		case core.RoleSystem, core.RoleDeveloper, core.RoleUser, core.RoleTool, core.RoleAssistant:
		default:
			return fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
		if msg.Role != core.RoleAssistant && len(m.ToolCalls) > 0 {
			return fmt.Errorf("messages[%d]: only assistant messages carry tool_calls", i)
		}
		if msg.Role != core.RoleTool && m.ToolCallID != "" {
			return fmt.Errorf("messages[%d]: only tool messages carry tool_call_id", i)
		}
		if isNull(m.Content) {
			if msg.Role != core.RoleAssistant || len(m.ToolCalls) == 0 {
				return fmt.Errorf("messages[%d]: content is required", i)
			}
		} else {
			parts, err := parseContent(m.Content)
			if err != nil {
				return fmt.Errorf("messages[%d]: %w", i, err)
			}
			size := 0
			for _, p := range parts {
				size += len(p.Text)
			}
			if size > maxMessageBytes {
				return fmt.Errorf("messages[%d]: content exceeds 1 MiB", i)
			}
			msg.Content = parts
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == "" || tc.Type != "function" || tc.Function.Name == "" || calls[tc.ID] {
				return fmt.Errorf("messages[%d]: each tool call needs a unique id, type function and a name", i)
			}
			calls[tc.ID] = true
			msg.ToolCalls = append(msg.ToolCalls, core.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		if msg.Role == core.RoleTool {
			if !calls[m.ToolCallID] {
				return fmt.Errorf("messages[%d]: tool_call_id %q answers no earlier tool call", i, m.ToolCallID)
			}
			msg.ToolCallID = m.ToolCallID
		}
		hasUser = hasUser || msg.Role == core.RoleUser
		req.Messages = append(req.Messages, msg)
	}
	if !hasUser {
		return errors.New("at least one user message is required")
	}
	return nil
}

// Response rendering in the OpenAI wire shape.

func wireMessage(out core.Completion) map[string]any {
	msg := map[string]any{"role": "assistant", "content": nil}
	if out.Text != "" || (len(out.ToolCalls) == 0 && out.Refusal == "") {
		msg["content"] = out.Text
	}
	if out.Refusal != "" {
		msg["refusal"] = out.Refusal
	}
	if len(out.ToolCalls) > 0 {
		calls := make([]any, len(out.ToolCalls))
		for i, c := range out.ToolCalls {
			calls[i] = map[string]any{"id": c.ID, "type": "function", "function": map[string]string{"name": c.Name, "arguments": c.Arguments}}
		}
		msg["tool_calls"] = calls
	}
	return msg
}

// wireDelta renders one chunk's visible content; nil when there is none.
func wireDelta(c core.Chunk) map[string]any {
	delta := map[string]any{}
	if c.Text != "" {
		delta["content"] = c.Text
	}
	if c.Refusal != "" {
		delta["refusal"] = c.Refusal
	}
	if len(c.ToolCalls) > 0 {
		calls := make([]any, len(c.ToolCalls))
		for i, d := range c.ToolCalls {
			call := map[string]any{"index": d.Index}
			fn := map[string]string{"arguments": d.Arguments}
			if d.ID != "" {
				call["id"], call["type"] = d.ID, "function"
			}
			if d.Name != "" {
				fn["name"] = d.Name
			}
			call["function"] = fn
			calls[i] = call
		}
		delta["tool_calls"] = calls
	}
	if len(delta) == 0 {
		return nil
	}
	return delta
}

// wireUsage renders usage the way OpenAI SDKs parse it.
func wireUsage(u *core.Usage) any {
	if u == nil {
		return nil
	}
	prompt := map[string]int64{"cached_tokens": u.CachedInput}
	if u.CacheWrite > 0 {
		prompt["cache_write_tokens"] = u.CacheWrite
	}
	return map[string]any{
		"prompt_tokens": u.InputTokens, "completion_tokens": u.OutputTokens, "total_tokens": u.TotalTokens,
		"prompt_tokens_details":     prompt,
		"completion_tokens_details": map[string]int64{"reasoning_tokens": u.ReasoningTokens},
	}
}
