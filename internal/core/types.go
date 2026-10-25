// Package core contains provider-independent request, result and usage types.
package core

import "time"

// This starter deliberately supports text messages only. Unsupported client
// fields are rejected, not silently discarded.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ChatRequest struct {
	Model               string         `json:"model"`
	Messages            []Message      `json:"messages"`
	Stream              bool           `json:"stream,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
	MaxCompletionTokens *int           `json:"max_completion_tokens,omitempty"`
	MaxTokens           *int           `json:"max_tokens,omitempty"`
	StreamOptions       *StreamOptions `json:"stream_options,omitempty"`
}

func (r ChatRequest) TokenLimit() int {
	if r.MaxCompletionTokens != nil {
		return *r.MaxCompletionTokens
	}
	if r.MaxTokens != nil {
		return *r.MaxTokens
	}
	return 512
}

type Usage struct {
	InputTokens     int64 `json:"prompt_tokens"`
	OutputTokens    int64 `json:"completion_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
	CachedInput     int64 `json:"cached_input_tokens,omitempty"`
	CacheWrite      int64 `json:"cache_write_tokens,omitempty"`
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
}

func (u *Usage) Normalize() {
	u.TotalTokens = u.InputTokens + u.OutputTokens
}

type Completion struct {
	Text         string
	FinishReason string
	Usage        *Usage // nil means unknown; NEVER interpret as zero tokens.
}

type Chunk struct {
	Text         string
	FinishReason string
	Usage        *Usage
}

type Model struct {
	ID       string `json:"id"`
	Provider string `json:"-"`
	Upstream string `json:"-"`
	// All pricing is for a million tokens and denominated in MICROUSD.
	// Nil means price is NOT configured. Do not substitute example pricing
	// for real provider pricing without checking the vendor's rates.
	Price *Price `json:"-"`
}

type Price struct {
	InputMicrosPerMillion      int64
	OutputMicrosPerMillion     int64
	CacheReadMicrosPerMillion  int64
	CacheWriteMicrosPerMillion int64
}

type UsageRecord struct {
	ID       string    `json:"id"`
	Model    string    `json:"model"`
	Provider string    `json:"provider"`
	Status   string    `json:"status"`
	Usage    *Usage    `json:"usage,omitempty"`
	CostUSD  *string   `json:"cost_usd,omitempty"`
	Time     time.Time `json:"time"`
}
