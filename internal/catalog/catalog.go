// Package catalog describes what the gateway serves: public models, the
// provider deployments behind them, and aliases such as router/auto. A model
// is what a caller asks for; a deployment is one place it runs (ADR-003).
package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"example.com/minirouter/internal/billing"
	"example.com/minirouter/internal/core"
)

// Config is the JSON catalog file. Secrets never appear in it: providers name
// the environment variable that holds their key.
type Config struct {
	Providers   []ProviderConfig `json:"providers"`
	Models      []Model          `json:"models"`
	Deployments []Deployment     `json:"deployments"`
	Aliases     []Alias          `json:"aliases"`
}

type ProviderConfig struct {
	Name      string `json:"name"`        // referenced by deployments
	Type      string `json:"type"`        // openai | openai_compatible | anthropic | gemini | demo
	BaseURL   string `json:"base_url"`    // required for openai_compatible
	APIKeyEnv string `json:"api_key_env"` // environment variable holding the key
	// MaxTokensField and DeveloperAsSystem adapt openai_compatible hosts.
	MaxTokensField    string   `json:"max_tokens_field"`
	DeveloperAsSystem bool     `json:"developer_as_system"`
	StreamIdleTimeout Duration `json:"stream_idle_timeout"`
}

type Capabilities struct {
	Tools            bool `json:"tools"`
	StructuredOutput bool `json:"structured_output"`
}

type Model struct {
	ID              string       `json:"id"` // public, e.g. "openai/gpt-4.1-mini"
	Capabilities    Capabilities `json:"capabilities"`
	ContextTokens   int          `json:"context_tokens"`
	MaxOutputTokens int          `json:"max_output_tokens"`
	Quality         float64      `json:"quality"` // operator-assigned 0–1, used by policy routing
}

type Deployment struct {
	ID             string     `json:"id"`
	Model          string     `json:"model"`
	Provider       string     `json:"provider"`
	UpstreamModel  string     `json:"upstream_model"`
	Priority       int        `json:"priority"`        // higher tiers are tried first
	Weight         int        `json:"weight"`          // share within a tier; 0 means 1
	MaxConcurrency int        `json:"max_concurrency"` // in-flight calls; 0 means unlimited
	Disabled       bool       `json:"disabled"`
	Price          *PriceCard `json:"price"` // nil: unknown, never free
}

// PriceCard is USD per million tokens as exact decimal strings. A price is
// billable only once VerifiedAt says someone checked it against the vendor.
type PriceCard struct {
	Input      string `json:"input"`
	Output     string `json:"output"`
	CacheRead  string `json:"cache_read"`  // empty: charged as input
	CacheWrite string `json:"cache_write"` // empty: charged as input
	// LongContext prices the whole request once input passes AboveInputTokens.
	LongContext *struct {
		AboveInputTokens int64  `json:"above_input_tokens"`
		Input            string `json:"input"`
		Output           string `json:"output"`
		CacheRead        string `json:"cache_read"`
		CacheWrite       string `json:"cache_write"`
	} `json:"long_context"`
	SourceURL  string `json:"source_url"`
	VerifiedAt string `json:"verified_at"` // RFC 3339; empty marks a draft
}

type Alias struct {
	ID     string   `json:"id"`     // e.g. "router/auto"
	Policy string   `json:"policy"` // "ordered": models in the listed order
	Models []string `json:"models"`
}

// Duration is a time.Duration written as "120s" in JSON.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("durations are strings such as \"30s\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil || v < 0 {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

// Parse decodes a catalog strictly: unknown fields are errors, so a typo can
// never silently disable a setting.
func Parse(data []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("catalog: %w", err)
	}
	return c, nil
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(data)
}

// Price converts a card to billing rates. Drafts and missing cards are nil:
// an unverified price is shown, never charged.
func (p *PriceCard) Price() (*core.Price, error) {
	if p == nil || p.VerifiedAt == "" {
		return nil, nil
	}
	if _, err := time.Parse(time.RFC3339, p.VerifiedAt); err != nil {
		return nil, fmt.Errorf("verified_at must be RFC 3339: %w", err)
	}
	price, err := rates(p.Input, p.Output, p.CacheRead, p.CacheWrite)
	if err != nil {
		return nil, err
	}
	if l := p.LongContext; l != nil {
		if l.AboveInputTokens <= 0 {
			return nil, errors.New("long_context.above_input_tokens must be positive")
		}
		if price.LongContext, err = rates(l.Input, l.Output, l.CacheRead, l.CacheWrite); err != nil {
			return nil, fmt.Errorf("long_context: %w", err)
		}
		price.LongContextThreshold = l.AboveInputTokens
	}
	return price, nil
}

func rates(input, output, cacheRead, cacheWrite string) (*core.Price, error) {
	var p core.Price
	var err error
	if p.InputMicrosPerMillion, err = billing.ParseUSD(input); err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	if p.OutputMicrosPerMillion, err = billing.ParseUSD(output); err != nil {
		return nil, fmt.Errorf("output: %w", err)
	}
	p.CacheReadMicrosPerMillion, p.CacheWriteMicrosPerMillion = p.InputMicrosPerMillion, p.InputMicrosPerMillion
	if cacheRead != "" {
		if p.CacheReadMicrosPerMillion, err = billing.ParseUSD(cacheRead); err != nil {
			return nil, fmt.Errorf("cache_read: %w", err)
		}
	}
	if cacheWrite != "" {
		if p.CacheWriteMicrosPerMillion, err = billing.ParseUSD(cacheWrite); err != nil {
			return nil, fmt.Errorf("cache_write: %w", err)
		}
	}
	return &p, nil
}

var providerTypes = map[string]bool{"openai": true, "openai_compatible": true, "anthropic": true, "gemini": true, "demo": true}

// Validate reports every problem at once, so one restart fixes them all.
func (c Config) Validate(getenv func(string) string) error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	providers := map[string]bool{}
	for _, p := range c.Providers {
		switch {
		case p.Name == "" || providers[p.Name]:
			bad("provider %q: names must be unique and non-empty", p.Name)
		case !providerTypes[p.Type]:
			bad("provider %q: unknown type %q", p.Name, p.Type)
		case p.Type == "openai_compatible" && p.BaseURL == "":
			bad("provider %q: openai_compatible needs base_url", p.Name)
		case p.Type != "demo" && (p.APIKeyEnv == "" || getenv(p.APIKeyEnv) == ""):
			bad("provider %q: set api_key_env to an environment variable holding the key (%q is empty)", p.Name, p.APIKeyEnv)
		}
		if p.BaseURL != "" {
			if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				bad("provider %q: base_url must be an http(s) URL", p.Name)
			}
		}
		if p.MaxTokensField != "" && p.MaxTokensField != "max_tokens" && p.MaxTokensField != "max_completion_tokens" {
			bad("provider %q: max_tokens_field must be max_tokens or max_completion_tokens", p.Name)
		}
		providers[p.Name] = true
	}
	models := map[string]bool{}
	for _, m := range c.Models {
		if m.ID == "" || models[m.ID] {
			bad("model %q: ids must be unique and non-empty", m.ID)
		}
		if m.ContextTokens < 0 || m.MaxOutputTokens < 0 || m.Quality < 0 || m.Quality > 1 {
			bad("model %q: token limits must be ≥ 0 and quality within 0–1", m.ID)
		}
		models[m.ID] = true
	}
	deployments, served := map[string]bool{}, map[string]bool{}
	for _, d := range c.Deployments {
		if d.ID == "" || deployments[d.ID] {
			bad("deployment %q: ids must be unique and non-empty", d.ID)
		}
		deployments[d.ID] = true
		if !models[d.Model] {
			bad("deployment %q: unknown model %q", d.ID, d.Model)
		}
		if !providers[d.Provider] {
			bad("deployment %q: unknown provider %q", d.ID, d.Provider)
		}
		if d.UpstreamModel == "" {
			bad("deployment %q: upstream_model is required", d.ID)
		}
		if d.Weight < 0 || d.MaxConcurrency < 0 {
			bad("deployment %q: weight and max_concurrency must be ≥ 0", d.ID)
		}
		if _, err := d.Price.Price(); err != nil {
			bad("deployment %q: price: %v", d.ID, err)
		}
		if !d.Disabled {
			served[d.Model] = true
		}
	}
	for id := range models {
		if !served[id] {
			bad("model %q: no enabled deployment serves it", id)
		}
	}
	aliases := map[string]bool{}
	for _, a := range c.Aliases {
		if a.ID == "" || aliases[a.ID] || models[a.ID] {
			bad("alias %q: ids must be unique and distinct from model ids", a.ID)
		}
		aliases[a.ID] = true
		if a.Policy != "ordered" {
			bad("alias %q: unknown policy %q", a.ID, a.Policy)
		}
		if len(a.Models) == 0 {
			bad("alias %q: lists no models", a.ID)
		}
		for _, m := range a.Models {
			if !models[m] {
				bad("alias %q: unknown model %q", a.ID, m)
			}
		}
	}
	return errors.Join(errs...)
}

// Snapshot is an immutable, indexed view of a validated catalog.
type Snapshot struct {
	Config
	Prices map[string]*core.Price // deployment id → billable price, nil if unknown
	models map[string]Model
	byID   map[string][]Deployment // model id → enabled deployments, config order
	alias  map[string]Alias
}

func NewSnapshot(c Config) (*Snapshot, error) {
	s := &Snapshot{Config: c, Prices: map[string]*core.Price{}, models: map[string]Model{}, byID: map[string][]Deployment{}, alias: map[string]Alias{}}
	for _, m := range c.Models {
		s.models[m.ID] = m
	}
	for _, d := range c.Deployments {
		price, err := d.Price.Price()
		if err != nil {
			return nil, err
		}
		s.Prices[d.ID] = price
		if !d.Disabled {
			s.byID[d.Model] = append(s.byID[d.Model], d)
		}
	}
	for _, a := range c.Aliases {
		s.alias[a.ID] = a
	}
	return s, nil
}

func (s *Snapshot) Model(id string) (Model, bool) {
	m, ok := s.models[id]
	return m, ok
}

// Deployments returns the enabled deployments of a model in config order.
func (s *Snapshot) Deployments(model string) []Deployment { return s.byID[model] }

func (s *Snapshot) Alias(id string) (Alias, bool) {
	a, ok := s.alias[id]
	return a, ok
}

// Registry holds the current snapshot; a reload is one atomic swap, so a
// request always sees one consistent catalog.
type Registry struct{ current atomic.Pointer[Snapshot] }

func NewRegistry(s *Snapshot) *Registry {
	r := &Registry{}
	r.current.Store(s)
	return r
}

func (r *Registry) Load() *Snapshot   { return r.current.Load() }
func (r *Registry) Store(s *Snapshot) { r.current.Store(s) }
