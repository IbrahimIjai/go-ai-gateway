// Package core contains provider-independent request, result and usage types.
package core

import (
	"encoding/json"
	"strings"
	"time"
)

// Role names the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one conversation turn. Content holds text parts; images are not
// supported yet and are rejected at the API, never silently dropped. An
// assistant turn may carry ToolCalls with or without text; a tool turn
// answers the call named by ToolCallID.
type Message struct {
	Role       Role
	Content    []Part
	ToolCalls  []ToolCall
	ToolCallID string
}

// Part is one piece of message content.
type Part struct {
	Text string
}

// Text joins the message's text parts.
func (m Message) Text() string {
	if len(m.Content) == 1 {
		return m.Content[0].Text
	}
	var b strings.Builder
	for _, p := range m.Content {
		b.WriteString(p.Text)
	}
	return b.String()
}

// TextMessage is a message with a single text part.
func TextMessage(role Role, text string) Message {
	return Message{Role: role, Content: []Part{{Text: text}}}
}

// ToolCall is a function call made by the model. Arguments is the JSON text
// exactly as the model produced it, so it can be replayed byte for byte.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Tool is a function the model may call.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage // a JSON Schema object; nil when the function takes none
	Strict      bool            // ask for exact schema adherence (not every provider can)
}

// ToolChoice constrains tool use. Mode is "auto", "none", "required" or
// "function", which forces a call to Name.
type ToolChoice struct {
	Mode string
	Name string
}

// ResponseFormat asks for JSON output. Type is "json_object" or "json_schema";
// plain text needs no ResponseFormat.
type ResponseFormat struct {
	Type   string
	Name   string          // json_schema only
	Schema json.RawMessage // json_schema only
	Strict bool            // json_schema only
}

// ChatRequest is a validated chat request in MiniRouter's own terms. The API
// layer builds it from the OpenAI wire shape; adapters translate it onward.
type ChatRequest struct {
	Model             string // public model ID or alias, as requested
	Messages          []Message
	Tools             []Tool
	ToolChoice        *ToolChoice // nil: provider default
	ParallelToolCalls *bool       // nil: provider default
	ResponseFormat    *ResponseFormat
	MaxOutputTokens   int
	Temperature       *float64
	Stop              []string
	Stream            bool
	IncludeUsage      bool
}

type Usage struct {
	InputTokens     int64 `json:"prompt_tokens"`
	OutputTokens    int64 `json:"completion_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
	CachedInput     int64 `json:"cached_input_tokens,omitempty"`
	CacheWrite      int64 `json:"cache_write_tokens,omitempty"`
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	// Raw is the provider's own usage object, kept for reconciliation.
	Raw json.RawMessage `json:"-"`
}

func (u *Usage) Normalize() {
	u.TotalTokens = u.InputTokens + u.OutputTokens
}

type Completion struct {
	Text         string
	Refusal      string // the model declined; OpenAI reports this apart from Text
	ToolCalls    []ToolCall
	FinishReason string
	Usage        *Usage // nil means unknown; NEVER interpret as zero tokens.
}

// Chunk is one streamed increment. A tool call arrives in pieces: the first
// delta for an Index carries ID and Name, later ones only argument text.
type Chunk struct {
	Text         string
	Refusal      string
	ToolCalls    []ToolCallDelta
	FinishReason string
	Usage        *Usage
}

// HasContent reports whether the chunk carries anything a client would see,
// as opposed to bookkeeping such as usage alone.
func (c Chunk) HasContent() bool {
	return c.Text != "" || c.Refusal != "" || len(c.ToolCalls) > 0 || c.FinishReason != ""
}

type ToolCallDelta struct {
	Index     int // position of the call within the response
	ID        string
	Name      string
	Arguments string // the next piece of argument JSON text
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
	// LongContext, when set, replaces these rates for requests whose input
	// exceeds LongContextThreshold tokens.
	LongContextThreshold int64
	LongContext          *Price
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
