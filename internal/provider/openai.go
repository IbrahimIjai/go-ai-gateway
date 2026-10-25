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

type OpenAI struct {
	Key, BaseURL string
	Client       *http.Client
	// StreamIdleTimeout cuts a stream off after this long without upstream
	// bytes; zero means 120 s.
	StreamIdleTimeout time.Duration
}

func (p *OpenAI) Name() string { return "openai" }
func (p *OpenAI) base() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return "https://api.openai.com"
}
func (p *OpenAI) payload(model string, req core.ChatRequest, stream bool) any {
	v := map[string]any{"model": model, "messages": req.Messages, "max_completion_tokens": req.TokenLimit(), "stream": stream}
	if req.Temperature != nil {
		v["temperature"] = *req.Temperature
	}
	if stream {
		v["stream_options"] = map[string]bool{"include_usage": true}
	}
	return v
}
func (p *OpenAI) send(ctx context.Context, model string, req core.ChatRequest, stream bool) (*http.Response, error) {
	return postJSON(ctx, defaultHTTP(p.Client), endpoint(p.base(), "/v1/chat/completions"), p.payload(model, req, stream), map[string]string{"Authorization": "Bearer " + p.Key})
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content   string          `json:"content"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		Input         int64 `json:"prompt_tokens"`
		Output        int64 `json:"completion_tokens"`
		PromptDetails struct {
			Cached int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		OutputDetails struct {
			Reasoning int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

func fromOpenAIUsage(raw *openAIResponse) *core.Usage {
	if raw.Usage == nil {
		return nil
	}
	u := &core.Usage{InputTokens: raw.Usage.Input, OutputTokens: raw.Usage.Output, CachedInput: raw.Usage.PromptDetails.Cached, ReasoningTokens: raw.Usage.OutputDetails.Reasoning}
	u.Normalize()
	return u
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
	if len(raw.Choices[0].Message.ToolCalls) > 0 && string(raw.Choices[0].Message.ToolCalls) != "null" {
		return core.Completion{}, fmt.Errorf("unsupported response: tool calls")
	}
	return core.Completion{Text: raw.Choices[0].Message.Content, FinishReason: raw.Choices[0].FinishReason, Usage: fromOpenAIUsage(&raw)}, nil
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
}

func (s *openAIStream) Close() error { return s.body.Close() }
func (s *openAIStream) Recv() (core.Chunk, error) {
	if s.done {
		return core.Chunk{}, io.EOF
	}
	for {
		e, err := s.events.Next()
		if err == io.EOF {
			return core.Chunk{}, io.ErrUnexpectedEOF // upstream must send [DONE]
		}
		if err != nil {
			return core.Chunk{}, err
		}
		if e.Data == "[DONE]" {
			s.done = true
			return core.Chunk{}, io.EOF
		}
		var v struct {
			Choices []struct {
				Delta struct {
					Content   string          `json:"content"`
					ToolCalls json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
				Finish string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				Input         int64 `json:"prompt_tokens"`
				Output        int64 `json:"completion_tokens"`
				PromptDetails struct {
					Cached int64 `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				OutputDetails struct {
					Reasoning int64 `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			} `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if err = json.Unmarshal([]byte(e.Data), &v); err != nil {
			return core.Chunk{}, fmt.Errorf("invalid openai SSE: %w", err)
		}
		if len(v.Error) > 0 && string(v.Error) != "null" {
			return core.Chunk{}, fmt.Errorf("upstream stream error")
		}
		var chunk core.Chunk
		if len(v.Choices) > 0 {
			if len(v.Choices[0].Delta.ToolCalls) > 0 && string(v.Choices[0].Delta.ToolCalls) != "null" {
				return core.Chunk{}, fmt.Errorf("unsupported streaming response: tool calls")
			}
			chunk.Text = v.Choices[0].Delta.Content
			chunk.FinishReason = v.Choices[0].Finish
		}
		if v.Usage != nil {
			chunk.Usage = &core.Usage{InputTokens: v.Usage.Input, OutputTokens: v.Usage.Output, CachedInput: v.Usage.PromptDetails.Cached, ReasoningTokens: v.Usage.OutputDetails.Reasoning}
			chunk.Usage.Normalize()
		}
		if strings.TrimSpace(e.Data) == "" {
			continue
		}
		if chunk.Text == "" && chunk.FinishReason == "" && chunk.Usage == nil {
			continue
		}
		return chunk, nil
	}
}
