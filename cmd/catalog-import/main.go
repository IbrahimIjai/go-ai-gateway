// Command catalog-import drafts catalog entries from LiteLLM's MIT-licensed
// model_prices_and_context_window.json. Prices are converted exactly and
// written as unverified drafts: they are published, never billed, until
// someone checks them against the vendor and sets verified_at.
//
//	catalog-import -prices model_prices_and_context_window.json gpt-4.1-mini claude-haiku-4-5
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"

	"example.com/minirouter/internal/catalog"
)

func main() {
	prices := flag.String("prices", "", "path to LiteLLM's model_prices_and_context_window.json")
	flag.Parse()
	if *prices == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: catalog-import -prices FILE MODEL...")
		os.Exit(2)
	}
	data, err := os.ReadFile(*prices)
	if err == nil {
		err = run(data, flag.Args(), os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "catalog-import:", err)
		os.Exit(1)
	}
}

type entry struct {
	Provider        string      `json:"litellm_provider"`
	MaxInput        json.Number `json:"max_input_tokens"`
	MaxOutput       json.Number `json:"max_output_tokens"`
	Input           json.Number `json:"input_cost_per_token"`
	Output          json.Number `json:"output_cost_per_token"`
	CacheRead       json.Number `json:"cache_read_input_token_cost"`
	CacheWrite      json.Number `json:"cache_creation_input_token_cost"`
	Tools           bool        `json:"supports_function_calling"`
	Structured      bool        `json:"supports_response_schema"`
	Source          string      `json:"source"`
	InputAbove200k  json.Number `json:"input_cost_per_token_above_200k_tokens"`
	OutputAbove200k json.Number `json:"output_cost_per_token_above_200k_tokens"`
}

func run(data []byte, ids []string, out io.Writer) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber() // keep "4e-07" as written; float64 would round it
	var all map[string]json.RawMessage
	if err := dec.Decode(&all); err != nil {
		return err
	}
	var cfg catalog.Config
	for _, id := range ids {
		raw, ok := all[id]
		if !ok {
			return fmt.Errorf("%s: not in the price file", id)
		}
		var e entry
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.UseNumber()
		if err := d.Decode(&e); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		card := &catalog.PriceCard{SourceURL: e.Source} // verified_at stays empty: a draft
		var err error
		for _, f := range []struct {
			dst *string
			src json.Number
		}{{&card.Input, e.Input}, {&card.Output, e.Output}, {&card.CacheRead, e.CacheRead}, {&card.CacheWrite, e.CacheWrite}} {
			if *f.dst, err = perMillion(f.src); err != nil {
				return fmt.Errorf("%s: %w", id, err)
			}
		}
		if e.InputAbove200k != "" {
			l := &struct {
				AboveInputTokens int64  `json:"above_input_tokens"`
				Input            string `json:"input"`
				Output           string `json:"output"`
				CacheRead        string `json:"cache_read"`
				CacheWrite       string `json:"cache_write"`
			}{AboveInputTokens: 200_000}
			if l.Input, err = perMillion(e.InputAbove200k); err == nil {
				l.Output, err = perMillion(e.OutputAbove200k)
			}
			if err != nil {
				return fmt.Errorf("%s: long context: %w", id, err)
			}
			card.LongContext = l
		}
		public := e.Provider + "/" + id
		cfg.Models = append(cfg.Models, catalog.Model{ID: public, Capabilities: catalog.Capabilities{Tools: e.Tools, StructuredOutput: e.Structured}, ContextTokens: atoi(e.MaxInput), MaxOutputTokens: atoi(e.MaxOutput)})
		cfg.Deployments = append(cfg.Deployments, catalog.Deployment{ID: public, Model: public, Provider: e.Provider, UpstreamModel: id, Price: card})
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(cfg)
}

// perMillion converts USD per token to USD per million tokens exactly, as a
// decimal string with at most 6 places. Empty stays empty (unknown).
func perMillion(n json.Number) (string, error) {
	if n == "" {
		return "", nil
	}
	r, ok := new(big.Rat).SetString(string(n))
	if !ok || r.Sign() < 0 {
		return "", fmt.Errorf("invalid price %q", n)
	}
	r.Mul(r, big.NewRat(1_000_000, 1))
	micro := new(big.Rat).Mul(r, big.NewRat(1_000_000, 1))
	if !micro.IsInt() {
		return "", fmt.Errorf("price %s is not exact to the micro-USD per million tokens", n)
	}
	s := r.FloatString(6)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s, nil
}

func atoi(n json.Number) int {
	v, _ := n.Int64()
	return int(v)
}
