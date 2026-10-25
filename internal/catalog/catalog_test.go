package catalog

import (
	"strings"
	"testing"
)

func keys(name string) string { return "k-" + name } // every key variable is set

func TestExampleCatalogIsValid(t *testing.T) {
	c, err := Load("../../config/minirouter.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(keys); err != nil {
		t.Fatal(err)
	}
	snap, err := NewSnapshot(c)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(snap.Deployments("open/llama-3.3-70b")); got != 2 {
		t.Fatalf("want one model on two hosts, got %d deployments", got)
	}
	if snap.Prices["openai/gpt-4.1-mini"] != nil {
		t.Fatal("an unverified (draft) price must never be billable")
	}
	if _, err := c.Build(keys); err != nil {
		t.Fatal(err)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	c, err := Parse([]byte(`{
	  "providers": [{"name": "x", "type": "openai_compatible", "api_key_env": "MISSING"}, {"name": "x", "type": "llama"}],
	  "models": [{"id": "m", "quality": 2}, {"id": "orphan"}],
	  "deployments": [{"id": "d", "model": "m", "provider": "nope", "upstream_model": "", "price": {"input": "1.2.3", "output": "1", "verified_at": "2026-10-25T00:00:00Z"}}],
	  "aliases": [{"id": "m", "policy": "magic", "models": ["ghost"]}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	err = c.Validate(func(string) string { return "" })
	for _, want := range []string{"needs base_url", "unknown type", "quality", `unknown provider "nope"`, "upstream_model is required", "price: input", `model "orphan": no enabled deployment`, "distinct from model ids", "unknown policy", `unknown model "ghost"`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	if _, err := Parse([]byte(`{"models": [{"id": "m", "max_ouput_tokens": 5}]}`)); err == nil {
		t.Fatal("a misspelled field must not be ignored")
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{"OPENAI_API_KEY": "k", "GEMINI_API_KEY": "k", "GEMINI_MODEL": "gemini-x",
		"OPENAI_INPUT_USD_PER_1M": "2", "OPENAI_OUTPUT_USD_PER_1M": "8", "GEMINI_INPUT_USD_PER_1M": "0.30", "GEMINI_OUTPUT_USD_PER_1M": "2.50"}
	c, err := FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := NewSnapshot(c)
	auto, _ := snap.Alias("router/auto")
	if strings.Join(auto.Models, ",") != "google/gemini-x,openai/gpt-4.1-mini" {
		t.Fatalf("want live models cheapest first, got %v", auto.Models)
	}
	if p := snap.Prices["openai/gpt-4.1-mini"]; p == nil || p.InputMicrosPerMillion != 2_000_000 {
		t.Fatalf("operator-set env prices are billable: %+v", p)
	}
	delete(env, "GEMINI_OUTPUT_USD_PER_1M")
	c, _ = FromEnv(func(k string) string { return env[k] })
	snap, _ = NewSnapshot(c)
	auto, _ = snap.Alias("router/auto")
	if strings.Join(auto.Models, ",") != "openai/gpt-4.1-mini,google/gemini-x" {
		t.Fatalf("with an unknown price, keep configured order (unknown is not free): %v", auto.Models)
	}
	if c, _ := FromEnv(func(string) string { return "" }); c.Aliases[0].Models[0] != "demo/echo" {
		t.Fatal("without provider keys router/auto serves the demo")
	}
}
