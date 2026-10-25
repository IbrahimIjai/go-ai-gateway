package catalog

import (
	"fmt"
	"sort"
	"time"

	"example.com/minirouter/internal/provider"
)

// Build creates one adapter per configured provider, each with its own
// HTTP client and so its own connection pool.
func (c Config) Build(getenv func(string) string) (map[string]provider.Provider, error) {
	out := map[string]provider.Provider{}
	for _, p := range c.Providers {
		key, idle := getenv(p.APIKeyEnv), time.Duration(p.StreamIdleTimeout)
		switch p.Type {
		case "openai", "openai_compatible":
			out[p.Name] = &provider.OpenAI{Key: key, BaseURL: p.BaseURL, Client: provider.NewHTTPClient(), MaxTokensField: p.MaxTokensField, DeveloperAsSystem: p.DeveloperAsSystem, StreamIdleTimeout: idle}
		case "anthropic":
			out[p.Name] = &provider.Anthropic{Key: key, BaseURL: p.BaseURL, Client: provider.NewHTTPClient(), StreamIdleTimeout: idle}
		case "gemini":
			out[p.Name] = &provider.Gemini{Key: key, BaseURL: p.BaseURL, Client: provider.NewHTTPClient(), StreamIdleTimeout: idle}
		case "demo":
			out[p.Name] = provider.Demo{}
		default:
			return nil, fmt.Errorf("provider %q: unknown type %q", p.Name, p.Type)
		}
	}
	return out, nil
}

// FromEnv builds the quick-start catalog from environment variables: the demo
// model, one model per provider key that is set, and router/auto over the live
// ones (the demo when there are none).
func FromEnv(getenv func(string) string) (Config, error) {
	env := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	c := Config{
		Providers:   []ProviderConfig{{Name: "demo", Type: "demo"}},
		Models:      []Model{{ID: "demo/echo", Capabilities: Capabilities{Tools: true}}},
		Deployments: []Deployment{{ID: "demo/echo", Model: "demo/echo", Provider: "demo", UpstreamModel: "echo", Price: &PriceCard{Input: "0", Output: "0", VerifiedAt: "2026-01-01T00:00:00Z"}}},
	}
	var live []string
	priced := true
	for _, p := range []struct{ name, typ, prefix, model string }{
		{"openai", "openai", "OPENAI", "gpt-4.1-mini"},
		{"anthropic", "anthropic", "ANTHROPIC", "claude-haiku-4-5"},
		{"google", "gemini", "GEMINI", "gemini-2.5-flash"},
	} {
		if getenv(p.prefix+"_API_KEY") == "" {
			continue
		}
		upstream := env(p.prefix+"_MODEL", p.model)
		id := p.name + "/" + upstream
		card := &PriceCard{Input: getenv(p.prefix + "_INPUT_USD_PER_1M"), Output: getenv(p.prefix + "_OUTPUT_USD_PER_1M"), CacheRead: getenv(p.prefix + "_CACHE_READ_USD_PER_1M"), CacheWrite: getenv(p.prefix + "_CACHE_WRITE_USD_PER_1M")}
		if card.Input == "" || card.Output == "" {
			card, priced = nil, false
		} else {
			card.VerifiedAt = time.Now().UTC().Format(time.RFC3339) // the operator set these rates
		}
		c.Providers = append(c.Providers, ProviderConfig{Name: p.name, Type: p.typ, APIKeyEnv: p.prefix + "_API_KEY"})
		c.Models = append(c.Models, Model{ID: id, Capabilities: Capabilities{Tools: true, StructuredOutput: p.typ != "anthropic"}})
		c.Deployments = append(c.Deployments, Deployment{ID: id, Model: id, Provider: p.name, UpstreamModel: upstream, Price: card})
		live = append(live, id)
	}
	if len(live) == 0 {
		live = []string{"demo/echo"}
	} else if priced {
		// Cheapest first, but only when every price is known: an unknown
		// price must never sort as free.
		cost := map[string]int64{}
		for _, d := range c.Deployments {
			if p, err := d.Price.Price(); err == nil && p != nil {
				cost[d.Model] = p.InputMicrosPerMillion + p.OutputMicrosPerMillion
			}
		}
		sort.SliceStable(live, func(i, j int) bool { return cost[live[i]] < cost[live[j]] })
	}
	c.Aliases = []Alias{{ID: "router/auto", Policy: "ordered", Models: live}}
	return c, c.Validate(getenv)
}

// Static is a minimal registry for tests: each id is a model served by one
// deployment on the provider of the same name, with router/auto over them all
// in order.
func Static(ids ...string) *Registry {
	var c Config
	for _, id := range ids {
		c.Models = append(c.Models, Model{ID: id, Capabilities: Capabilities{Tools: true}})
		c.Deployments = append(c.Deployments, Deployment{ID: id, Model: id, Provider: id, UpstreamModel: id})
	}
	c.Aliases = []Alias{{ID: "router/auto", Policy: "ordered", Models: ids}}
	snap, err := NewSnapshot(c)
	if err != nil {
		panic(err)
	}
	return NewRegistry(snap)
}
