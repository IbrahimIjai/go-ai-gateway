package provider

import (
	"encoding/json"
	"strings"
)

// jsonObject returns a tool call's arguments as a JSON object, which Anthropic
// and Gemini require; OpenAI alone accepts any string. Empty means {}.
func jsonObject(provider, args string) (json.RawMessage, error) {
	if strings.TrimSpace(args) == "" {
		return json.RawMessage(`{}`), nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &obj) != nil || obj == nil {
		return nil, &UnsupportedError{Provider: provider, Feature: "tool call arguments that are not a JSON object"}
	}
	return json.RawMessage(args), nil
}

// argumentsText renders tool input that arrived as a JSON value. No input
// means a call without arguments, which OpenAI clients expect as "{}".
func argumentsText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	return string(raw)
}
