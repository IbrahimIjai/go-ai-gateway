package router

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"example.com/minirouter/internal/catalog"
	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/health"
	"example.com/minirouter/internal/provider"
)

type spec struct {
	id      string
	caps    catalog.Capabilities
	quality float64
	in, out string // USD per million; empty: unpriced
	context int
}

// aliased builds router/x with the given policy over models served by one
// deployment each.
func aliased(policy string, specs ...spec) *Router {
	c := catalog.Config{}
	providers := map[string]provider.Provider{}
	var ids []string
	for _, s := range specs {
		c.Models = append(c.Models, catalog.Model{ID: s.id, Capabilities: s.caps, Quality: s.quality, ContextTokens: s.context})
		d := catalog.Deployment{ID: s.id, Model: s.id, Provider: s.id, UpstreamModel: s.id}
		if s.in != "" {
			d.Price = &catalog.PriceCard{Input: s.in, Output: s.out, VerifiedAt: "2026-10-25T00:00:00Z"}
		}
		c.Deployments = append(c.Deployments, d)
		providers[s.id] = &script{}
		ids = append(ids, s.id)
	}
	c.Aliases = []catalog.Alias{{ID: "router/x", Policy: policy, Models: ids}}
	snap, err := catalog.NewSnapshot(c)
	if err != nil {
		panic(err)
	}
	return &Router{Catalog: catalog.NewRegistry(snap), Providers: providers, Health: health.New(health.Defaults, nil),
		Rand: func() float64 { return 0 }, Sleep: noWait}
}

func order(t *testing.T, r *Router, req core.ChatRequest) ([]string, core.RouteDecision) {
	t.Helper()
	d, plan, err := r.Explain(req)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range plan {
		ids = append(ids, c.Deployment.ID)
	}
	return ids, d
}

func chat(model string) core.ChatRequest {
	return core.ChatRequest{Model: model, MaxOutputTokens: 100, Messages: []core.Message{core.TextMessage(core.RoleUser, "hi")}}
}

func TestGatesFailClosed(t *testing.T) {
	r := aliased("ordered", spec{id: "plain"}, spec{id: "tooly", caps: catalog.Capabilities{Tools: true}})
	req := chat("router/x")
	req.Tools = []core.Tool{{Name: "f"}}
	if ids, d := order(t, r, req); strings.Join(ids, ",") != "tooly" || !strings.Contains(strings.Join(d.Reasons, ","), "excluded plain: no tool support") {
		t.Fatalf("tools need a tool-capable model: %v %v", ids, d.Reasons)
	}
	req.ToolChoice = &core.ToolChoice{Mode: "none"}
	if ids, _ := order(t, r, req); len(ids) != 2 {
		t.Fatal("tool_choice none needs no tool support")
	}
	req = chat("plain")
	req.ResponseFormat = &core.ResponseFormat{Type: "json_object"}
	_, _, err := r.Explain(req)
	var none *NoEligibleError
	if !errors.As(err, &none) || !strings.Contains(err.Error(), "plain: no structured output") {
		t.Fatalf("an exact model that cannot comply is refused with the reason: %v", err)
	}
	if _, _, _, err := r.Complete(context.Background(), req); !errors.As(err, &none) {
		t.Fatal("Complete applies the same gate")
	}
}

func TestContextGate(t *testing.T) {
	r := aliased("ordered", spec{id: "small", context: 1000}, spec{id: "big", context: 100000})
	req := chat("router/x")
	req.Messages = []core.Message{core.TextMessage(core.RoleUser, strings.Repeat("x", 8000))}
	if ids, _ := order(t, r, req); strings.Join(ids, ",") != "big" {
		t.Fatalf("~2000 input tokens do not fit a 1000-token context: %v", ids)
	}
}

func TestEcoPicksCheapestBoundedCostAndSkipsUnpriced(t *testing.T) {
	r := aliased("eco",
		spec{id: "cheap-in-pricey-out", in: "0.10", out: "20"},
		spec{id: "balanced", in: "1", out: "2"},
		spec{id: "mystery"})
	ids, d := order(t, r, chat("router/x"))
	// For 100 output tokens the output price dominates the bound.
	if strings.Join(ids, ",") != "balanced,cheap-in-pricey-out" || !strings.Contains(strings.Join(d.Reasons, ","), "mystery: unpriced") {
		t.Fatalf("eco: %v %v", ids, d.Reasons)
	}
}

func TestFastFollowsObservedLatency(t *testing.T) {
	r := aliased("fast", spec{id: "slow"}, spec{id: "quick"}, spec{id: "new"})
	for id, took := range map[string]time.Duration{"slow": 900 * time.Millisecond, "quick": 100 * time.Millisecond} {
		k, _ := r.Health.Acquire(id, 0)
		k.Record("", took, false, false)
		k.Release()
	}
	if ids, _ := order(t, r, chat("router/x")); ids[0] != "quick" || ids[1] != "new" || ids[2] != "slow" {
		t.Fatalf("fast: quickest first; an unmeasured model gets the neutral prior, above a slow one: %v", ids)
	}
}

func TestAutoWeighsQualityCostSpeedReliability(t *testing.T) {
	r := aliased("auto",
		spec{id: "good-dear", quality: 0.9, in: "0.2", out: "0.6"},
		spec{id: "ok-cheap", quality: 0.5, in: "0.1", out: "0.3"})
	ids, d := order(t, r, chat("router/x"))
	// Twice the price for much better quality wins...
	if ids[0] != "good-dear" || len(d.Scores) != 2 || d.Scores[0].Cost != 0.5 || d.Scores[1].Cost != 1 {
		t.Fatalf("auto: %v %+v", ids, d.Scores)
	}
	// ...but not a hundred times the price.
	r = aliased("auto",
		spec{id: "good-dear", quality: 0.9, in: "10", out: "30"},
		spec{id: "ok-cheap", quality: 0.5, in: "0.1", out: "0.3"})
	if ids, _ := order(t, r, chat("router/x")); ids[0] != "ok-cheap" {
		t.Fatalf("auto: 100× the price outweighs quality: %v", ids)
	}
	raw, _ := json.Marshal(d)
	if !strings.Contains(string(raw), `"scores":[{"model":"good-dear"`) {
		t.Fatalf("scores are part of the decision: %s", raw)
	}
}

func TestContextLengthErrorMovesToNextModel(t *testing.T) {
	r := aliased("ordered", spec{id: "small"}, spec{id: "big"})
	r.Providers["small"] = &script{errs: []error{&provider.APIError{Status: 400, Code: "context_length_exceeded"}}}
	c, _, d, err := r.Complete(context.Background(), chat("router/x"))
	if err != nil || c.Model.ID != "big" || !strings.Contains(strings.Join(d.Reasons, ","), "failover_context_length:small") {
		t.Fatalf("%s %v %v", c.Model.ID, d.Reasons, err)
	}
}
