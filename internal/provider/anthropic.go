package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"example.com/minirouter/internal/core"
)

type Anthropic struct {
	Key, BaseURL string
	Client       *http.Client
}

func (p *Anthropic) Name() string { return "anthropic" }
func (p *Anthropic) base() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return "https://api.anthropic.com"
}
func (p *Anthropic) payload(model string, req core.ChatRequest, stream bool) any {
	var system []string
	messages := make([]core.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			system = append(system, m.Content)
			continue
		}
		messages = append(messages, m)
	}
	v := map[string]any{"model": model, "messages": messages, "max_tokens": req.TokenLimit(), "stream": stream}
	if len(system) > 0 {
		v["system"] = strings.Join(system, "\n\n")
	}
	if req.Temperature != nil {
		v["temperature"] = *req.Temperature
	}
	return v
}
func (p *Anthropic) send(ctx context.Context, model string, req core.ChatRequest, stream bool) (*http.Response, error) {
	return postJSON(ctx, defaultHTTP(p.Client), endpoint(p.base(), "/v1/messages"), p.payload(model, req, stream), map[string]string{"x-api-key": p.Key, "anthropic-version": "2023-06-01"})
}

// Anthropic returns cache-read and cache-created tokens separately from
// ordinary input_tokens. Normalize InputTokens as their sum.
type anthropicUsage struct {
	Input  int64 `json:"input_tokens"`
	Output int64 `json:"output_tokens"`
	Read   int64 `json:"cache_read_input_tokens"`
	Write  int64 `json:"cache_creation_input_tokens"`
}

func (u anthropicUsage) core() *core.Usage {
	v := &core.Usage{InputTokens: u.Input + u.Read + u.Write, OutputTokens: u.Output, CachedInput: u.Read, CacheWrite: u.Write}
	v.Normalize()
	return v
}
func anthroFinish(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return s
	}
}
func (p *Anthropic) Complete(ctx context.Context, model string, req core.ChatRequest) (core.Completion, error) {
	resp, err := p.send(ctx, model, req, false)
	if err != nil {
		return core.Completion{}, err
	}
	var raw struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string         `json:"stop_reason"`
		Usage      anthropicUsage `json:"usage"`
	}
	raw, err = readJSON[struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string         `json:"stop_reason"`
		Usage      anthropicUsage `json:"usage"`
	}](resp)
	if err != nil {
		return core.Completion{}, err
	}
	var text strings.Builder
	for _, c := range raw.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		} else {
			return core.Completion{}, fmt.Errorf("unsupported anthropic response block: %s", c.Type)
		}
	}
	return core.Completion{Text: text.String(), FinishReason: anthroFinish(raw.StopReason), Usage: raw.Usage.core()}, nil
}
func (p *Anthropic) Stream(ctx context.Context, model string, req core.ChatRequest) (Stream, error) {
	resp, err := p.send(ctx, model, req, true)
	if err != nil {
		return nil, err
	}
	return &anthroStream{body: resp.Body, events: newEventReader(resp.Body)}, nil
}

type anthroStream struct {
	body     io.ReadCloser
	events   *eventReader
	usage    anthropicUsage
	hasUsage bool
	finished bool
}

func (s *anthroStream) Close() error { return s.body.Close() }
func (s *anthroStream) Recv() (core.Chunk, error) {
	if s.finished {
		return core.Chunk{}, io.EOF
	}
	for {
		e, err := s.events.Next()
		if err == io.EOF {
			return core.Chunk{}, io.ErrUnexpectedEOF
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
		case "content_block_delta":
			var v struct {
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(e.Data), &v); err != nil {
				return core.Chunk{}, err
			}
			if v.Delta.Type == "text_delta" && v.Delta.Text != "" {
				return core.Chunk{Text: v.Delta.Text}, nil
			}
			if v.Delta.Type != "" && v.Delta.Type != "text_delta" {
				return core.Chunk{}, fmt.Errorf("unsupported anthropic streaming block: %s", v.Delta.Type)
			}
		case "message_delta":
			var v struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					Output int64 `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(e.Data), &v); err != nil {
				return core.Chunk{}, err
			}
			s.usage.Output = v.Usage.Output
			c := core.Chunk{FinishReason: anthroFinish(v.Delta.StopReason)}
			if s.hasUsage {
				c.Usage = s.usage.core()
			}
			return c, nil
		case "message_stop":
			s.finished = true
			return core.Chunk{}, io.EOF
		case "error":
			return core.Chunk{}, fmt.Errorf("anthropic upstream streaming error")
		}
	}
}
