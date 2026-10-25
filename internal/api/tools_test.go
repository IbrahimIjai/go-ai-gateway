package api

import (
	"encoding/json"
	"strings"
	"testing"
)

const weatherTool = `{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}`

func TestToolRequestValidation(t *testing.T) {
	s := makeTestServer()
	user := `{"role":"user","content":"hi"}`
	cases := []struct {
		name, body, want string
	}{
		{"tool_choice without tools", `{"model":"demo/echo","messages":[` + user + `],"tool_choice":"auto"}`, "tool_choice requires tools"},
		{"forced unknown tool", `{"model":"demo/echo","messages":[` + user + `],"tools":[` + weatherTool + `],"tool_choice":{"type":"function","function":{"name":"nope"}}}`, "tool_choice must"},
		{"bad tool name", `{"model":"demo/echo","messages":[` + user + `],"tools":[{"type":"function","function":{"name":"has space"}}]}`, "tool name"},
		{"orphan tool result", `{"model":"demo/echo","messages":[` + user + `,{"role":"tool","tool_call_id":"call_x","content":"1"}]}`, "answers no earlier tool call"},
		{"image input", `{"model":"demo/echo","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`, "image input is not supported"},
		{"null user content", `{"model":"demo/echo","messages":[{"role":"user","content":null}]}`, "content is required"},
		{"bad schema format", `{"model":"demo/echo","messages":[` + user + `],"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":[]}}}`, "json_schema needs"},
		{"too many stops", `{"model":"demo/echo","messages":[` + user + `],"stop":["a","b","c","d","e"]}`, "at most 4"},
	}
	for _, tc := range cases {
		rec := testPost(t, s, tc.body, "Bearer local-test-key")
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: want 400 containing %q, got %d %s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}
}

// The demo model calls a tool, then answers from its result, so a whole tool
// loop runs through the real handlers offline.
func TestToolLoopThroughTheAPI(t *testing.T) {
	s := makeTestServer()
	rec := testPost(t, s, `{"model":"demo/echo","messages":[{"role":"user","content":[{"type":"text","text":"Weather?"}]}],"tools":[`+weatherTool+`]}`, "Bearer local-test-key")
	var first struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Details struct {
				Cached *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil || rec.Code != 200 {
		t.Fatalf("first turn: %d %s", rec.Code, rec.Body.String())
	}
	msg := first.Choices[0].Message
	if msg.Content != nil || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Type != "function" || msg.ToolCalls[0].Function.Name != "get_weather" || first.Choices[0].FinishReason != "tool_calls" || first.Usage.Details.Cached == nil {
		t.Fatalf("want one tool call, null content, OpenAI-shaped usage: %s", rec.Body.String())
	}
	id := msg.ToolCalls[0].ID
	second := `{"model":"demo/echo","messages":[{"role":"user","content":"Weather?"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"` + id + `","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"` + id + `","content":"21C"}],"tools":[` + weatherTool + `],"stream":true}`
	rec = testPost(t, s, second, "Bearer local-test-key")
	if body := rec.Body.String(); rec.Code != 200 || !strings.Contains(body, `"content":"21C"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("second turn: %d %s", rec.Code, body)
	}
}

func TestStreamedToolCallDeltas(t *testing.T) {
	s := makeTestServer()
	rec := testPost(t, s, `{"model":"demo/echo","messages":[{"role":"user","content":"go"}],"tools":[`+weatherTool+`],"tool_choice":"required","stream":true}`, "Bearer local-test-key")
	body := rec.Body.String()
	want := `"tool_calls":[{"function":{"arguments":"{}","name":"get_weather"},"id":"call_demo_1","index":0,"type":"function"}]`
	if !strings.Contains(body, want) || !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("missing OpenAI-shaped tool call delta: %s", body)
	}
}
