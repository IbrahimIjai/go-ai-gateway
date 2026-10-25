package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/minirouter/internal/core"
)

func testReq() core.ChatRequest {
	return core.ChatRequest{Messages: []core.Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hello"}}}
}

func TestOpenAICompleteAndStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("invalid request: %s", r.URL.String())
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n")
			io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	p := &OpenAI{Key: "test-key", BaseURL: srv.URL, Client: srv.Client()}
	c, err := p.Complete(context.Background(), "fake-model", testReq())
	if err != nil || c.Text != "hello" || c.Usage.TotalTokens != 6 {
		t.Fatalf("unexpected %v %+v", err, c)
	}
	st, err := p.Stream(context.Background(), "fake-model", testReq())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	first, err := st.Recv()
	if err != nil || first.Text != "hello" {
		t.Fatalf("bad first chunk: %v %#v", err, first)
	}
	second, err := st.Recv()
	if err != nil || second.Usage.TotalTokens != 6 {
		t.Fatalf("bad usage chunk: %v %#v", err, second)
	}
	_, err = st.Recv()
	if err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}
func TestAnthropicTranslate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-version") == "" {
			t.Error("missing anthropic-version")
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["system"] != "be brief" {
			t.Errorf("invalid system mapping: %#v", b["system"])
		}
		if b["stream"] == true {
			io.WriteString(w, "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":2,\"cache_read_input_tokens\":3}}}\n\n")
			io.WriteString(w, "event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
			io.WriteString(w, "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")
			io.WriteString(w, "event: message_stop\ndata: {}\n\n")
			return
		}
		io.WriteString(w, `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"cache_read_input_tokens":3,"output_tokens":1}}`)
	}))
	defer srv.Close()
	p := &Anthropic{Key: "key", BaseURL: srv.URL, Client: srv.Client()}
	c, err := p.Complete(context.Background(), "model", testReq())
	if err != nil || c.Text != "hi" || c.Usage.InputTokens != 5 || c.Usage.CachedInput != 3 {
		t.Fatalf("bad nonstream: %+v %v", c, err)
	}
	st, err := p.Stream(context.Background(), "model", testReq())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	part, _ := st.Recv()
	if part.Text != "hi" {
		t.Errorf("bad text: %+v", part)
	}
	end, err := st.Recv()
	if err != nil || end.FinishReason != "stop" || end.Usage.TotalTokens != 6 {
		t.Fatalf("bad finish: %+v %v", end, err)
	}
}
func TestGeminiTranslate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "gemini-test") || r.Header.Get("x-goog-api-key") != "key" {
			t.Errorf("bad request %v", r.URL)
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if _, ok := b["systemInstruction"]; !ok {
			t.Errorf("missing systemInstruction: %#v", b)
		}
		resp := `{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"thoughtsTokenCount":1}}`
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			io.WriteString(w, "data: "+resp+"\n\n")
		} else {
			io.WriteString(w, resp)
		}
	}))
	defer srv.Close()
	p := &Gemini{Key: "key", BaseURL: srv.URL, Client: srv.Client()}
	c, err := p.Complete(context.Background(), "gemini-test", testReq())
	if err != nil || c.Usage.OutputTokens != 3 || c.Usage.ReasoningTokens != 1 {
		t.Fatalf("bad completion: %+v %v", c, err)
	}
	st, err := p.Stream(context.Background(), "gemini-test", testReq())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	chunk, err := st.Recv()
	if err != nil || chunk.Text != "hello" || chunk.Usage.TotalTokens != 6 {
		t.Fatalf("bad stream: %+v %v", chunk, err)
	}
}
func TestSSEMultilineData(t *testing.T) {
	e, err := newEventReader(strings.NewReader("event: message\ndata: first\ndata: second\n\n")).Next()
	if err != nil || e.Name != "message" || e.Data != "first\nsecond" {
		t.Fatalf("bad SSE event: %+v %v", e, err)
	}
}

// A stream is finished only by its provider's end marker: OpenAI [DONE],
// Anthropic message_stop, Gemini finishReason. A cut connection must surface
// as io.ErrUnexpectedEOF, never as a clean io.EOF.
func TestStreamsRequireEndMarker(t *testing.T) {
	adapters := []struct {
		name           string
		build          func(url string, c *http.Client) Provider
		clean, cutLate string
	}{
		{"openai", func(u string, c *http.Client) Provider { return &OpenAI{Key: "k", BaseURL: u, Client: c} },
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"},
		{"anthropic", func(u string, c *http.Client) Provider { return &Anthropic{Key: "k", BaseURL: u, Client: c} },
			"event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: message_stop\ndata: {}\n\n",
			"event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"},
		{"gemini", func(u string, c *http.Client) Provider { return &Gemini{Key: "k", BaseURL: u, Client: c} },
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}]}\n\n",
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n"},
	}
	for _, a := range adapters {
		for _, tc := range []struct {
			name, body string
			want       error
		}{
			{"clean", a.clean, io.EOF},
			{"cut after first delta", a.cutLate, io.ErrUnexpectedEOF},
			{"cut before any event", "", io.ErrUnexpectedEOF},
		} {
			t.Run(a.name+"/"+tc.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.body) }))
				defer srv.Close()
				st, err := a.build(srv.URL, srv.Client()).Stream(context.Background(), "m", testReq())
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
				for err == nil {
					_, err = st.Recv()
				}
				if err != tc.want {
					t.Fatalf("want %v, got %v", tc.want, err)
				}
			})
		}
	}
}

// Gemini answers a blocked prompt with promptFeedback and no candidates. That
// is a finished response (content_filter), not an error and not a cut stream.
func TestGeminiBlockedPromptIsContentFilter(t *testing.T) {
	blocked := `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":4}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			io.WriteString(w, "data: "+blocked+"\n\n")
			return
		}
		io.WriteString(w, blocked)
	}))
	defer srv.Close()
	p := &Gemini{Key: "k", BaseURL: srv.URL, Client: srv.Client()}
	c, err := p.Complete(context.Background(), "m", testReq())
	if err != nil || c.FinishReason != "content_filter" || c.Usage == nil {
		t.Fatalf("non-stream: %+v %v", c, err)
	}
	st, err := p.Stream(context.Background(), "m", testReq())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if chunk, err := st.Recv(); err != nil || chunk.FinishReason != "content_filter" {
		t.Fatalf("stream: %+v %v", chunk, err)
	}
	if _, err := st.Recv(); err != io.EOF {
		t.Fatalf("want clean EOF after block, got %v", err)
	}
}
