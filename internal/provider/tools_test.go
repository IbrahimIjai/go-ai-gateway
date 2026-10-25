package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"example.com/minirouter/internal/core"
)

// toolConversation is a canonical two-call tool loop: one JSON result and one
// plain-text result, as OpenAI clients send them.
func toolConversation() core.ChatRequest {
	return core.ChatRequest{
		Model: "m", MaxOutputTokens: 256,
		Messages: []core.Message{
			core.TextMessage(core.RoleSystem, "Be brief."),
			core.TextMessage(core.RoleUser, "Weather in Paris and Rome?"),
			{Role: core.RoleAssistant, Content: []core.Part{{Text: "Checking."}}, ToolCalls: []core.ToolCall{
				{ID: "call_1", Name: "get_weather", Arguments: `{"city":"Paris"}`},
				{ID: "call_2", Name: "get_weather", Arguments: `{"city":"Rome"}`},
			}},
			{Role: core.RoleTool, ToolCallID: "call_1", Content: []core.Part{{Text: `{"temp_c":21}`}}},
			{Role: core.RoleTool, ToolCallID: "call_2", Content: []core.Part{{Text: "sunny"}}},
		},
		Tools:      []core.Tool{{Name: "get_weather", Description: "Current weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}},
		ToolChoice: &core.ToolChoice{Mode: "auto"},
	}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// serve answers every request with body, recording the last request body.
func serve(t *testing.T, body string, got *map[string]any) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			_ = json.NewDecoder(r.Body).Decode(got)
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sameJSON(t *testing.T, want string, got map[string]any) {
	t.Helper()
	var w map[string]any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, got) {
		g, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("request differs from fixture; got:\n%s", g)
	}
}

func TestToolConversationRequests(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		build   func(string, *http.Client) Provider
	}{
		{"openai_request.json", func(u string, c *http.Client) Provider { return &OpenAI{Key: "k", BaseURL: u, Client: c} }},
		{"anthropic_request.json", func(u string, c *http.Client) Provider { return &Anthropic{Key: "k", BaseURL: u, Client: c} }},
		{"gemini_request.json", func(u string, c *http.Client) Provider { return &Gemini{Key: "k", BaseURL: u, Client: c} }},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			var got map[string]any
			srv := serve(t, `{}`, &got)
			_, _ = tc.build(srv.URL, srv.Client()).Complete(context.Background(), "m", toolConversation())
			sameJSON(t, fixture(t, tc.fixture), got)
		})
	}
}

func TestOpenAIToolCalls(t *testing.T) {
	srv := serve(t, fixture(t, "openai_tool_calls.json"), nil)
	p := &OpenAI{Key: "k", BaseURL: srv.URL, Client: srv.Client()}
	c, err := p.Complete(context.Background(), "m", testReq())
	want := []core.ToolCall{{ID: "call_a", Name: "get_weather", Arguments: `{"city":"Paris"}`}, {ID: "call_b", Name: "get_weather", Arguments: `{"city":"Rome"}`}}
	if err != nil || !reflect.DeepEqual(c.ToolCalls, want) || c.FinishReason != "tool_calls" || c.Usage.CachedInput != 64 || len(c.Usage.Raw) == 0 {
		t.Fatalf("bad completion: %+v %v", c, err)
	}
	srv = serve(t, fixture(t, "openai_tool_calls.sse"), nil)
	p.BaseURL, p.Client = srv.URL, srv.Client()
	calls, finish := collectStream(t, p)
	if !reflect.DeepEqual(calls, want) || finish != "tool_calls" {
		t.Fatalf("bad stream: %+v %s", calls, finish)
	}
}

func TestAnthropicToolUse(t *testing.T) {
	srv := serve(t, fixture(t, "anthropic_tool_use.json"), nil)
	p := &Anthropic{Key: "k", BaseURL: srv.URL, Client: srv.Client()}
	c, err := p.Complete(context.Background(), "m", testReq())
	want := []core.ToolCall{{ID: "toolu_a", Name: "get_weather", Arguments: `{"city":"Paris"}`}, {ID: "toolu_b", Name: "get_time", Arguments: `{}`}}
	if err != nil || c.Text != "Checking." || !reflect.DeepEqual(c.ToolCalls, want) || c.FinishReason != "tool_calls" || c.Usage.InputTokens != 80 {
		t.Fatalf("bad completion: %+v %v", c, err)
	}
	srv = serve(t, fixture(t, "anthropic_tool_use.sse"), nil)
	p.BaseURL, p.Client = srv.URL, srv.Client()
	want[0].Arguments = `{"city": "Paris"}` // streamed bytes are kept as sent
	calls, finish := collectStream(t, p)
	if !reflect.DeepEqual(calls, want) || finish != "tool_calls" {
		t.Fatalf("bad stream: %+v %s", calls, finish)
	}
}

func TestGeminiFunctionCalls(t *testing.T) {
	srv := serve(t, fixture(t, "gemini_function_call.json"), nil)
	p := &Gemini{Key: "k", BaseURL: srv.URL, Client: srv.Client()}
	c, err := p.Complete(context.Background(), "m", testReq())
	if err != nil || c.Text != "Checking." || len(c.ToolCalls) != 2 || c.FinishReason != "tool_calls" || c.Usage.OutputTokens != 30 {
		t.Fatalf("bad completion: %+v %v", c, err)
	}
	checkGeminiCalls(t, c.ToolCalls)
	srv = serve(t, fixture(t, "gemini_function_call.sse"), nil)
	p.BaseURL, p.Client = srv.URL, srv.Client()
	calls, finish := collectStream(t, p)
	if finish != "tool_calls" {
		t.Fatalf("want tool_calls, got %s", finish)
	}
	checkGeminiCalls(t, calls)
}

// checkGeminiCalls verifies synthesized IDs are unique and the thought
// signature survives a round trip back into a Gemini request.
func checkGeminiCalls(t *testing.T, calls []core.ToolCall) {
	t.Helper()
	if calls[0].ID == calls[1].ID || !strings.HasPrefix(calls[1].ID, syntheticIDPrefix) || calls[0].Arguments != `{"city":"Paris"}` {
		t.Fatalf("bad calls: %+v", calls)
	}
	req := core.ChatRequest{MaxOutputTokens: 8, Messages: []core.Message{
		core.TextMessage(core.RoleUser, "go"),
		{Role: core.RoleAssistant, ToolCalls: calls},
		{Role: core.RoleTool, ToolCallID: calls[0].ID, Content: []core.Part{{Text: "21"}}},
		{Role: core.RoleTool, ToolCallID: calls[1].ID, Content: []core.Part{{Text: "22"}}},
	}}
	body, err := (&Gemini{}).payload(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(body)
	s := string(raw)
	if !strings.Contains(s, `"thoughtSignature":"c2lnbmF0dXJlLWE="`) || strings.Contains(s, syntheticIDPrefix) || !strings.Contains(s, `"name":"get_weather","response":{"content":"21"}`) {
		t.Fatalf("bad replay: %s", s)
	}
}

// collectStream assembles streamed tool call deltas the way a client would.
func collectStream(t *testing.T, p Provider) ([]core.ToolCall, string) {
	t.Helper()
	st, err := p.Stream(context.Background(), "m", testReq())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var calls []core.ToolCall
	finish := ""
	for {
		c, err := st.Recv()
		if err == io.EOF {
			return calls, finish
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range c.ToolCalls {
			for len(calls) <= d.Index {
				calls = append(calls, core.ToolCall{})
			}
			if d.ID != "" {
				calls[d.Index].ID, calls[d.Index].Name = d.ID, d.Name
			}
			calls[d.Index].Arguments += d.Arguments
		}
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
	}
}

func TestUnsupportedFeaturesAreRefused(t *testing.T) {
	strict := toolConversation()
	strict.Tools[0].Strict = true
	badArgs := toolConversation()
	badArgs.Messages[2].ToolCalls[0].Arguments = "not json"
	schema := toolConversation()
	schema.ResponseFormat = &core.ResponseFormat{Type: "json_object"}
	for _, tc := range []struct {
		name string
		p    Provider
		req  core.ChatRequest
	}{
		{"anthropic strict", &Anthropic{}, strict},
		{"gemini strict", &Gemini{}, strict},
		{"anthropic bad arguments", &Anthropic{}, badArgs},
		{"gemini bad arguments", &Gemini{}, badArgs},
		{"anthropic response_format", &Anthropic{}, schema},
	} {
		_, err := tc.p.Complete(context.Background(), "m", tc.req)
		if Class(err) != ClassUnsupported || !Retryable(err) {
			t.Errorf("%s: want unsupported (failover-able), got %v", tc.name, err)
		}
	}
}

func TestAnthropicToolIDsAreSanitizedConsistently(t *testing.T) {
	id := "call:abc/1"
	got := anthropicToolID(id)
	if got != anthropicToolID(id) || strings.ContainsAny(got, ":/") || got == anthropicToolID("call:abc/2") {
		t.Fatalf("bad sanitized id %q", got)
	}
	if anthropicToolID("toolu_01-ok") != "toolu_01-ok" {
		t.Fatal("valid ids must pass through")
	}
}

func TestInStreamErrorsAreClassified(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    func(string, *http.Client) Provider
		body string
		want FailureClass
	}{
		{"anthropic overloaded", func(u string, c *http.Client) Provider { return &Anthropic{BaseURL: u, Client: c} },
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n", ClassTransient},
		{"openai server error", func(u string, c *http.Client) Provider { return &OpenAI{BaseURL: u, Client: c} },
			"data: {\"error\":{\"message\":\"boom\",\"type\":\"server_error\",\"code\":null}}\n\n", ClassTransient},
		{"gemini unavailable", func(u string, c *http.Client) Provider { return &Gemini{BaseURL: u, Client: c} },
			"data: {\"error\":{\"code\":503,\"message\":\"overloaded\",\"status\":\"UNAVAILABLE\"}}\n\n", ClassTransient},
		{"gemini quota", func(u string, c *http.Client) Provider { return &Gemini{BaseURL: u, Client: c} },
			"data: {\"error\":{\"code\":429,\"message\":\"exhausted\",\"status\":\"RESOURCE_EXHAUSTED\"}}\n\n", ClassRateLimit},
	} {
		srv := serve(t, tc.body, nil)
		st, err := tc.p(srv.URL, srv.Client()).Stream(context.Background(), "m", testReq())
		if err == nil {
			_, err = st.Recv()
			st.Close()
		}
		if Class(err) != tc.want {
			t.Errorf("%s: want %s, got %s (%v)", tc.name, tc.want, Class(err), err)
		}
	}
}

// An OpenAI-compatible host gets its own base path, max_tokens and no
// developer role; its stream may end on a finish reason without [DONE].
func TestOpenAICompatibleHost(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer srv.Close()
	p := &OpenAI{Key: "k", BaseURL: srv.URL + "/inference/v1", Client: srv.Client(), MaxTokensField: "max_tokens", DeveloperAsSystem: true}
	req := testReq()
	req.Messages[0].Role = core.RoleDeveloper
	st, err := p.Stream(context.Background(), "m", req)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for err == nil {
		_, err = st.Recv()
	}
	if err != io.EOF || path != "/inference/v1/chat/completions" || got["max_tokens"] != float64(64) || got["max_completion_tokens"] != nil {
		t.Fatalf("err %v path %s body %v", err, path, got)
	}
	if role := got["messages"].([]any)[0].(map[string]any)["role"]; role != "system" {
		t.Fatalf("developer role sent as %v", role)
	}
}
