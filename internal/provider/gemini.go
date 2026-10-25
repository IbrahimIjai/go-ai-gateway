package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"example.com/minirouter/internal/core"
)

type Gemini struct {
	Key, BaseURL string
	Client       *http.Client
}

func (p *Gemini) Name() string { return "google" }
func (p *Gemini) base() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return "https://generativelanguage.googleapis.com"
}
func (p *Gemini) payload(req core.ChatRequest) any {
	var system []string
	var contents []any
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			system = append(system, m.Content)
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]any{"role": role, "parts": []any{map[string]string{"text": m.Content}}})
	}
	v := map[string]any{"contents": contents, "generationConfig": map[string]any{"maxOutputTokens": req.TokenLimit()}}
	if len(system) > 0 {
		v["systemInstruction"] = map[string]any{"parts": []any{map[string]string{"text": strings.Join(system, "\n\n")}}}
	}
	if req.Temperature != nil {
		v["generationConfig"].(map[string]any)["temperature"] = *req.Temperature
	}
	return v
}
func (p *Gemini) send(ctx context.Context, model string, req core.ChatRequest, stream bool) (*http.Response, error) {
	action := ":generateContent"
	suffix := ""
	if stream {
		action = ":streamGenerateContent"
		suffix = "?alt=sse"
	}
	path := "/v1beta/models/" + url.PathEscape(model) + action + suffix
	return postJSON(ctx, defaultHTTP(p.Client), endpoint(p.base(), path), p.payload(req), map[string]string{"x-goog-api-key": p.Key})
}

type geminiUsage struct {
	Input    int64 `json:"promptTokenCount"`
	Output   int64 `json:"candidatesTokenCount"`
	Thoughts int64 `json:"thoughtsTokenCount"`
	Cached   int64 `json:"cachedContentTokenCount"`
}

func (u *geminiUsage) core() *core.Usage {
	if u == nil {
		return nil
	}
	v := &core.Usage{InputTokens: u.Input, OutputTokens: u.Output + u.Thoughts, CachedInput: u.Cached, ReasoningTokens: u.Thoughts}
	v.Normalize()
	return v
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text         string          `json:"text"`
				FunctionCall json.RawMessage `json:"functionCall"`
				InlineData   json.RawMessage `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
		Finish string `json:"finishReason"`
	} `json:"candidates"`
	Usage          *geminiUsage    `json:"usageMetadata"`
	PromptFeedback json.RawMessage `json:"promptFeedback"`
}

func gemFinish(s string) string {
	switch s {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	default:
		return strings.ToLower(s)
	}
}
func gemChunk(v geminiResponse) (core.Chunk, error) {
	c := core.Chunk{Usage: v.Usage.core()}
	if len(v.Candidates) > 0 {
		for _, part := range v.Candidates[0].Content.Parts {
			if (len(part.FunctionCall) > 0 && string(part.FunctionCall) != "null") || (len(part.InlineData) > 0 && string(part.InlineData) != "null") {
				return core.Chunk{}, fmt.Errorf("unsupported gemini response part (tool/media)")
			}
			c.Text += part.Text
		}
		c.FinishReason = gemFinish(v.Candidates[0].Finish)
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
	if len(v.Candidates) == 0 {
		return core.Completion{}, fmt.Errorf("gemini returned no candidate")
	}
	c, err := gemChunk(v)
	if err != nil {
		return core.Completion{}, err
	}
	return core.Completion{Text: c.Text, FinishReason: c.FinishReason, Usage: c.Usage}, nil
}
func (p *Gemini) Stream(ctx context.Context, model string, req core.ChatRequest) (Stream, error) {
	resp, err := p.send(ctx, model, req, true)
	if err != nil {
		return nil, err
	}
	return &geminiStream{body: resp.Body, events: newEventReader(resp.Body)}, nil
}

type geminiStream struct {
	body     io.ReadCloser
	events   *eventReader
	finished bool
}

func (s *geminiStream) Close() error { return s.body.Close() }
func (s *geminiStream) Recv() (core.Chunk, error) {
	if s.finished {
		return core.Chunk{}, io.EOF
	}
	for {
		e, err := s.events.Next()
		if err == io.EOF {
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
		c, err := gemChunk(v)
		if err != nil {
			return core.Chunk{}, err
		}
		if c.Text != "" || c.FinishReason != "" || c.Usage != nil {
			return c, nil
		}
	}
}
