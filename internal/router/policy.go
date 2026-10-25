package router

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"example.com/minirouter/internal/core"
)

// NoEligibleError means no model can honor the request. Requirements come
// from protocol facts, and the gate fails closed (ADR-010): the caller learns
// why each candidate was excluded instead of an incapable model being tried.
type NoEligibleError struct {
	Requested string
	Why       []string // "model: reason"
}

func (e *NoEligibleError) Error() string {
	return "no model behind " + e.Requested + " can serve this request: " + strings.Join(e.Why, "; ")
}

// estimateInput is a rough input token count for routing, not billing:
// about four bytes per token plus a little per message and tool.
func estimateInput(req core.ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += 8 + len(m.Text())/4
		for _, c := range m.ToolCalls {
			n += (len(c.Name) + len(c.Arguments)) / 4
		}
	}
	for _, t := range req.Tools {
		n += 8 + (len(t.Name)+len(t.Description)+len(t.Parameters))/4
	}
	return n
}

// gate returns why a model cannot serve the request, or "" if it can.
func gate(c Candidate, req core.ChatRequest, input int) string {
	m := c.Model
	switch {
	case len(req.Tools) > 0 && (req.ToolChoice == nil || req.ToolChoice.Mode != "none") && !m.Capabilities.Tools:
		return "no tool support"
	case req.ResponseFormat != nil && !m.Capabilities.StructuredOutput:
		return "no structured output"
	case m.MaxOutputTokens > 0 && req.MaxOutputTokens > m.MaxOutputTokens:
		return fmt.Sprintf("max output %d < requested %d", m.MaxOutputTokens, req.MaxOutputTokens)
	case m.ContextTokens > 0 && float64(input+req.MaxOutputTokens) > 0.9*float64(m.ContextTokens):
		return fmt.Sprintf("context %d too small for ~%d tokens", m.ContextTokens, input+req.MaxOutputTokens)
	}
	return ""
}

// groups splits candidates into per-model runs, keeping order.
func groups(cands []Candidate) [][]Candidate {
	var out [][]Candidate
	for start := 0; start < len(cands); {
		end := start
		for end < len(cands) && cands[end].Model.ID == cands[start].Model.ID {
			end++
		}
		out = append(out, cands[start:end])
		start = end
	}
	return out
}

// boundedCost is what a request could cost at a model's cheapest priced
// deployment: estimated input plus the full output allowance. Unknown prices
// are never treated as free; ok is false then.
func boundedCost(g []Candidate, req core.ChatRequest, input int) (micros float64, ok bool) {
	best := math.Inf(1)
	for _, c := range g {
		if p := c.Price; p != nil {
			cost := float64(input)*float64(p.InputMicrosPerMillion)/1e6 + float64(req.MaxOutputTokens)*float64(p.OutputMicrosPerMillion)/1e6
			best = min(best, cost)
		}
	}
	return best, !math.IsInf(best, 1)
}

// observed is a model's median latency at its best deployment: time to first
// token for streams, total time otherwise. ok is false with no samples.
func (r *Router) observed(g []Candidate, stream bool) (time.Duration, bool) {
	best, ok := time.Duration(math.MaxInt64), false
	for _, c := range g {
		st := r.tracker().Stats(c.Deployment.ID)
		samples := st.Latency
		if stream {
			samples = st.TTFT
		}
		if len(samples) == 0 {
			continue
		}
		s := append([]time.Duration(nil), samples...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		best, ok = min(best, s[len(s)/2]), true
	}
	return best, ok
}

// reliability is the share of successful calls this minute across a model's
// deployments; with no calls yet it is 1.
func (r *Router) reliability(g []Candidate) float64 {
	ok, fail := 0, 0
	for _, c := range g {
		st := r.tracker().Stats(c.Deployment.ID)
		ok, fail = ok+st.OKThisMinute, fail+st.FailThisMin
	}
	if ok+fail == 0 {
		return 1
	}
	return float64(ok) / float64(ok+fail)
}

// Weights for router/auto, after router-core's auto profile with capability
// left out (the gate already enforces it).
const wQuality, wCost, wSpeed, wReliability = 0.5, 0.25, 0.15, 0.10

// latencyBuffer lets router/fast spread traffic over models within 10 % of
// the fastest instead of herding onto one (LiteLLM's lowest_latency_buffer).
const latencyBuffer = 0.10

// rank orders gated model groups for an alias policy and records why.
func (r *Router) rank(policy string, gs [][]Candidate, req core.ChatRequest, input int, d *core.RouteDecision) ([][]Candidate, error) {
	if policy == "ordered" || len(gs) < 2 {
		return gs, nil
	}
	type scored struct {
		g     []Candidate
		score core.Score
		cost  float64
		lat   time.Duration
		hasL  bool
	}
	var items []scored
	var why []string
	for _, g := range gs {
		cost, priced := boundedCost(g, req, input)
		if policy == "eco" && !priced {
			why = append(why, g[0].Model.ID+": unpriced (eco never treats unknown as free)")
			continue
		}
		lat, hasL := r.observed(g, req.Stream)
		items = append(items, scored{g: g, cost: cost, lat: lat, hasL: hasL,
			score: core.Score{Model: g[0].Model.ID, Quality: g[0].Model.Quality, Reliability: r.reliability(g)}})
	}
	if len(items) == 0 {
		return nil, &NoEligibleError{Requested: req.Model, Why: why}
	}
	d.Reasons = append(d.Reasons, why...)
	// Score cost and speed as ratios to the best (2× the cost scores 0.5), so
	// the size of a difference counts, not just its sign. Unknowns sit at 0.5.
	minC, minL := math.Inf(1), time.Duration(math.MaxInt64)
	for _, it := range items {
		if !math.IsInf(it.cost, 1) {
			minC = min(minC, it.cost)
		}
		if it.hasL {
			minL = min(minL, it.lat)
		}
	}
	for i := range items {
		it := &items[i]
		it.score.Cost, it.score.Speed = 0.5, 0.5
		if !math.IsInf(it.cost, 1) {
			it.score.Cost = ratio(minC, it.cost)
		}
		if it.hasL {
			it.score.Speed = ratio(float64(minL), float64(it.lat))
		}
		switch policy {
		case "eco":
			it.score.Total = it.score.Cost
		case "fast":
			it.score.Total = it.score.Speed
		default:
			it.score.Total = wQuality*it.score.Quality + wCost*it.score.Cost + wSpeed*it.score.Speed + wReliability*it.score.Reliability
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].score.Total > items[j].score.Total })
	if policy == "fast" && items[0].hasL {
		// Pick the lead at random among models within the buffer of the best.
		near := 1
		for near < len(items) && items[near].hasL && float64(items[near].lat) <= float64(items[0].lat)*(1+latencyBuffer) {
			near++
		}
		k := int(r.random() * float64(near))
		items[0], items[k] = items[k], items[0]
	}
	out := make([][]Candidate, len(items))
	for i, it := range items {
		out[i] = it.g
		d.Scores = append(d.Scores, it.score)
	}
	d.Reasons = append(d.Reasons, "ranked_"+policy)
	return out, nil
}

// ratio is best/v within 0–1; a zero best (free) makes every non-free value 0.
func ratio(best, v float64) float64 {
	if v <= best {
		return 1
	}
	return best / v
}
