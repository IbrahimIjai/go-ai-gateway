package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"example.com/minirouter/internal/catalog"
)

func TestPerMillionIsExact(t *testing.T) {
	for in, want := range map[string]string{"4e-07": "0.4", "1.6e-06": "1.6", "3e-08": "0.03", "0.000015": "15", "0": "0"} {
		if got, err := perMillion(json.Number(in)); err != nil || got != want {
			t.Errorf("%s: want %s, got %s %v", in, want, got, err)
		}
	}
	if _, err := perMillion("1e-13"); err == nil {
		t.Error("a price finer than a micro-USD per million must be refused, not rounded")
	}
}

func TestImportWritesDrafts(t *testing.T) {
	prices := `{"gpt-4.1-mini": {"litellm_provider": "openai", "max_input_tokens": 1047576, "max_output_tokens": 32768,
	  "input_cost_per_token": 4e-07, "output_cost_per_token": 1.6e-06, "cache_read_input_token_cost": 1e-07,
	  "supports_function_calling": true, "supports_response_schema": true, "source": "https://openai.com/api/pricing"}}`
	var out bytes.Buffer
	if err := run([]byte(prices), []string{"gpt-4.1-mini"}, &out); err != nil {
		t.Fatal(err)
	}
	cfg, err := catalog.Parse(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Deployments[0]
	if d.ID != "openai/gpt-4.1-mini" || d.Price.Input != "0.4" || d.Price.Output != "1.6" || d.Price.CacheRead != "0.1" || d.Price.VerifiedAt != "" {
		t.Fatalf("bad draft: %+v", d.Price)
	}
	if p, _ := d.Price.Price(); p != nil {
		t.Fatal("a draft must not be billable")
	}
	if !cfg.Models[0].Capabilities.StructuredOutput || cfg.Models[0].ContextTokens != 1047576 {
		t.Fatalf("bad model: %+v", cfg.Models[0])
	}
	if err := run([]byte(prices), []string{"missing"}, &out); err == nil || !strings.Contains(err.Error(), "not in the price file") {
		t.Fatalf("want a clear error, got %v", err)
	}
}
