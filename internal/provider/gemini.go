package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"example.com/minirouter/internal/core"
)

type Gemini struct {
	Key, BaseURL string
	Client       *http.Client
	// StreamIdleTimeout cuts a stream off after this long without upstream
	// bytes; zero means 120 s.
	StreamIdleTimeout time.Duration
}

func (p *Gemini) Name() string { return "google" }
func (p *Gemini) base() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return "https://generativelanguage.googleapis.com"
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	Thought          bool                    `json:"thought,omitempty"`
	ThoughtSignature string                  `json:"thoughtSignature,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	InlineData       json.RawMessage         `json:"inlineData,omitempty"`
}

type geminiFunctionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

// Gemini thinking models return a thoughtSignature with each function call
// and need it back when the call is replayed. OpenAI-shaped clients keep only
// the call ID, so the signature rides inside it (Bifrost's scheme).
const signatureSeparator = "_ts_"

// Gemini may omit call IDs. Synthesized ones carry this prefix so they are
// never sent back to Gemini as if it had issued them.
const syntheticIDPrefix = "call_mr_"

func syntheticID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return syntheticIDPrefix + hex.EncodeToString(b[:])
}

func joinSignature(id, sig string) string {
	if sig == "" {
		return id
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(sig); err != nil {
			return id // not base64: cannot carry it safely
		}
	}
	return id + signatureSeparator + base64.RawURLEncoding.EncodeToString(raw)
}

func splitSignature(id string) (base, sig string) {
	base, enc, found := strings.Cut(id, signatureSeparator)
	if !found {
		return id, ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return id, ""
	}
	return base, base64.StdEncoding.EncodeToString(raw)
}

// geminiID is the ID to send back to Gemini: its own, never ours.
func geminiID(id string) string {
	if strings.HasPrefix(id, syntheticIDPrefix) {
		return ""
	}
	return id
}

// functionResponse shapes a tool result for Gemini, which requires a JSON
// object: an object passes through, anything else is wrapped as
// {"content": "..."} (as Bifrost does; objects holding "$ref" are wrapped too).
func functionResponse(text string) json.RawMessage {
	var buf bytes.Buffer
	if json.Compact(&buf, []byte(text)) == nil && buf.Len() > 0 && buf.Bytes()[0] == '{' && !bytes.Contains(buf.Bytes(), []byte(`"$ref"`)) {
		return buf.Bytes()
	}
	if strings.TrimSpace(text) == "" {
		return json.RawMessage(`{}`)
	}
	wrapped, _ := json.Marshal(map[string]string{"content": text})
	return wrapped
}

func (p *Gemini) payload(req core.ChatRequest) (any, error) {
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		return nil, &UnsupportedError{Provider: "gemini", Feature: "parallel_tool_calls=false"}
	}
	var system []string
	var contents []geminiContent
	names := map[string]string{} // tool call ID → function name, for results
	for i := 0; i < len(req.Messages); i++ {
		m := req.Messages[i]
		switch m.Role {
		case core.RoleSystem, core.RoleDeveloper:
			system = append(system, m.Text())
		case core.RoleTool:
			// Parallel results go back together, as functionResponse parts only.
			var parts []geminiPart
			for ; i < len(req.Messages) && req.Messages[i].Role == core.RoleTool; i++ {
				t := req.Messages[i]
				id, _ := splitSignature(t.ToolCallID)
				parts = append(parts, geminiPart{FunctionResponse: &geminiFunctionResponse{ID: geminiID(id), Name: names[t.ToolCallID], Response: functionResponse(t.Text())}})
			}
			i--
			contents = append(contents, geminiContent{Role: "user", Parts: parts})
		case core.RoleAssistant:
			var parts []geminiPart
			for _, part := range m.Content {
				if part.Text != "" {
					parts = append(parts, geminiPart{Text: part.Text})
				}
			}
			for _, c := range m.ToolCalls {
				args, err := jsonObject("gemini", c.Arguments)
				if err != nil {
					return nil, err
				}
				id, sig := splitSignature(c.ID)
				names[c.ID] = c.Name
				parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{ID: geminiID(id), Name: c.Name, Args: args}, ThoughtSignature: sig})
			}
			contents = append(contents, geminiContent{Role: "model", Parts: parts})
		default:
			var parts []geminiPart
			for _, part := range m.Content {
				if part.Text != "" {
					parts = append(parts, geminiPart{Text: part.Text})
				}
			}
			contents = append(contents, geminiContent{Role: "user", Parts: parts})
		}
	}
	gen := map[string]any{"maxOutputTokens": req.MaxOutputTokens}
	if req.Temperature != nil {
		gen["temperature"] = *req.Temperature
	}
	if len(req.Stop) > 0 {
		gen["stopSequences"] = req.Stop
	}
	if f := req.ResponseFormat; f != nil {
		gen["responseMimeType"] = "application/json"
		if f.Type == "json_schema" {
			gen["responseJsonSchema"] = f.Schema
		}
	}
	v := map[string]any{"contents": contents, "generationConfig": gen}
	if len(system) > 0 {
		v["systemInstruction"] = map[string]any{"parts": []any{map[string]string{"text": strings.Join(system, "\n\n")}}}
	}
	if len(req.Tools) > 0 {
		decls := make([]any, len(req.Tools))
		for i, t := range req.Tools {
			if t.Strict {
				return nil, &UnsupportedError{Provider: "gemini", Feature: "strict tool schemas"}
			}
			d := map[string]any{"name": t.Name}
			if t.Description != "" {
				d["description"] = t.Description
			}
			if t.Parameters != nil {
				d["parametersJsonSchema"] = t.Parameters // plain JSON Schema, no dialect conversion
			}
			decls[i] = d
		}
		v["tools"] = []any{map[string]any{"functionDeclarations": decls}}
	}
	if c := req.ToolChoice; c != nil {
		cfg := map[string]any{}
		switch c.Mode {
		case "none":
			cfg["mode"] = "NONE"
		case "required":
			cfg["mode"] = "ANY"
		case "function":
			cfg["mode"], cfg["allowedFunctionNames"] = "ANY", []string{c.Name}
		default:
			cfg["mode"] = "AUTO"
		}
		v["toolConfig"] = map[string]any{"functionCallingConfig": cfg}
	}
	return v, nil
}
func (p *Gemini) send(ctx context.Context, model string, req core.ChatRequest, stream bool) (*http.Response, error) {
	body, err := p.payload(req)
	if err != nil {
		return nil, err
	}
	action := ":generateContent"
	suffix := ""
	if stream {
		action = ":streamGenerateContent"
		suffix = "?alt=sse"
	}
	path := "/v1beta/models/" + url.PathEscape(model) + action + suffix
	return postJSON(ctx, defaultHTTP(p.Client), endpoint(p.base(), path), body, map[string]string{"x-goog-api-key": p.Key})
}

type geminiUsage struct {
	Input    int64 `json:"promptTokenCount"`
	Output   int64 `json:"candidatesTokenCount"`
	Thoughts int64 `json:"thoughtsTokenCount"`
	Cached   int64 `json:"cachedContentTokenCount"`
}

func geminiUsageFrom(raw json.RawMessage) (*core.Usage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var u geminiUsage
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, fmt.Errorf("invalid gemini usage: %w", err)
	}
	// Thinking tokens are billed as output.
	v := &core.Usage{InputTokens: u.Input, OutputTokens: u.Output + u.Thoughts, CachedInput: u.Cached, ReasoningTokens: u.Thoughts, Raw: raw}
	v.Normalize()
	return v, nil
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
		Finish string `json:"finishReason"`
	} `json:"candidates"`
	Usage          json.RawMessage `json:"usageMetadata"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"` // set when the prompt was blocked; no candidates follow
	} `json:"promptFeedback"`
	Error json.RawMessage `json:"error"` // stream error event
}

// gemFinish follows Bifrost's mapping of Gemini finish reasons.
func gemFinish(s string) string {
	switch s {
	case "":
		return ""
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION":
		return "content_filter"
	default: // STOP, OTHER, MALFORMED_FUNCTION_CALL, ...
		return "stop"
	}
}

// gemChunk converts one response. next numbers tool calls across a stream.
func gemChunk(v geminiResponse, next *int) (core.Chunk, error) {
	usage, err := geminiUsageFrom(v.Usage)
	if err != nil {
		return core.Chunk{}, err
	}
	c := core.Chunk{Usage: usage}
	if len(v.Candidates) > 0 {
		for _, part := range v.Candidates[0].Content.Parts {
			switch {
			case part.Thought: // thought summaries are never requested or shown
			case part.FunctionCall != nil:
				id := part.FunctionCall.ID
				if id == "" {
					id = syntheticID() // the function name would collide on parallel calls
				}
				c.ToolCalls = append(c.ToolCalls, core.ToolCallDelta{Index: *next, ID: joinSignature(id, part.ThoughtSignature), Name: part.FunctionCall.Name, Arguments: argumentsText(part.FunctionCall.Args)})
				*next++
			case len(part.InlineData) > 0 && string(part.InlineData) != "null":
				return core.Chunk{}, fmt.Errorf("unsupported gemini response part: inline media")
			default:
				c.Text += part.Text
			}
		}
		c.FinishReason = gemFinish(v.Candidates[0].Finish)
	} else if v.PromptFeedback.BlockReason != "" {
		c.FinishReason = "content_filter"
	}
	return c, nil
}
func (p *Gemini) Complete(ctx context.Context, model string, req core.ChatRequest) (core.Completion, error) {
	resp, err := p.send(ctx, model, req, false)
	if err != nil {
		return core.Completion{}, err
	}
	v, err := readJSON[geminiResponse](resp)
	if err != nil {
		return core.Completion{}, err
	}
	if len(v.Candidates) == 0 && v.PromptFeedback.BlockReason == "" {
		return core.Completion{}, fmt.Errorf("gemini returned no candidate")
	}
	next := 0
	c, err := gemChunk(v, &next)
	if err != nil {
		return core.Completion{}, err
	}
	out := core.Completion{Text: c.Text, FinishReason: c.FinishReason, Usage: c.Usage}
	for _, d := range c.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, core.ToolCall{ID: d.ID, Name: d.Name, Arguments: d.Arguments})
	}
	if len(out.ToolCalls) > 0 && out.FinishReason == "stop" {
		out.FinishReason = "tool_calls" // Gemini says STOP even when it calls functions
	}
	return out, nil
}
func (p *Gemini) Stream(ctx context.Context, model string, req core.ChatRequest) (Stream, error) {
	body, err := openStream(ctx, p.StreamIdleTimeout, func(ctx context.Context) (*http.Response, error) {
		return p.send(ctx, model, req, true)
	})
	if err != nil {
		return nil, err
	}
	return &geminiStream{body: body, events: newEventReader(body)}, nil
}

type geminiStream struct {
	body     io.ReadCloser
	events   *eventReader
	finished bool
	// Gemini sends no [DONE]: a finish reason (or a blocked prompt) is the only
	// sign the answer is complete, so EOF before one is a cut connection.
	sawFinish bool
	next      int  // index for the next tool call
	calledFn  bool // a function call arrived, so STOP means tool_calls
}

func (s *geminiStream) Close() error { return s.body.Close() }
func (s *geminiStream) Recv() (core.Chunk, error) {
	if s.finished {
		return core.Chunk{}, io.EOF
	}
	for {
		e, err := s.events.Next()
		if err == io.EOF {
			if !s.sawFinish {
				return core.Chunk{}, io.ErrUnexpectedEOF
			}
			s.finished = true
			return core.Chunk{}, io.EOF
		}
		if err != nil {
			return core.Chunk{}, err
		}
		var v geminiResponse
		if err := json.Unmarshal([]byte(e.Data), &v); err != nil {
			return core.Chunk{}, fmt.Errorf("invalid gemini SSE: %w", err)
		}
		if len(v.Error) > 0 && string(v.Error) != "null" {
			return core.Chunk{}, newStreamError([]byte(e.Data))
		}
		c, err := gemChunk(v, &s.next)
		if err != nil {
			return core.Chunk{}, err
		}
		if len(c.ToolCalls) > 0 {
			s.calledFn = true
		}
		if c.FinishReason != "" {
			s.sawFinish = true
			if c.FinishReason == "stop" && s.calledFn {
				c.FinishReason = "tool_calls"
			}
		}
		if c.HasContent() || c.Usage != nil {
			return c, nil
		}
	}
}
