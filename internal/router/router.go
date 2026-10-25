// Package router resolves public model IDs, plans attempts across
// deployments, and retries safely before any downstream stream bytes.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"example.com/minirouter/internal/catalog"
	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/health"
	"example.com/minirouter/internal/provider"
)

var ErrUnknownModel = errors.New("unknown or unavailable model")

// Policy bounds the attempt loop.
type Policy struct {
	MaxAttempts    int           // upstream calls per request, failovers included
	SameRetries    int           // extra tries on one deployment after a transient failure
	BackoffInitial time.Duration // doubled per retry, ±20 % jitter (Bifrost)
	BackoffMax     time.Duration
	MaxWait        time.Duration // longest single wait, Retry-After included (Portkey)
}

var DefaultPolicy = Policy{MaxAttempts: 4, SameRetries: 1, BackoffInitial: 500 * time.Millisecond, BackoffMax: 5 * time.Second, MaxWait: 20 * time.Second}

type Router struct {
	Catalog   *catalog.Registry
	Providers map[string]provider.Provider
	Health    *health.Tracker // nil: a default tracker is created on first use
	Policy    Policy          // zero: DefaultPolicy
	// FirstEventTimeout bounds the wait for a stream's first event on every
	// attempt that still has a fallback behind it. The last attempt always
	// waits, so a slow answer beats no answer (Bifrost's rule). Zero disables.
	FirstEventTimeout time.Duration
	// Rand and Sleep are injectable for deterministic tests.
	Rand  func() float64
	Sleep func(context.Context, time.Duration) error
	once  sync.Once
}

// Candidate is one way to serve a request: a model at one deployment.
type Candidate struct {
	Model      catalog.Model
	Deployment catalog.Deployment
	Provider   provider.Provider
	Price      *core.Price // nil: unknown
	only       bool        // the model's only deployment
}

func (r *Router) policy() Policy {
	if r.Policy.MaxAttempts == 0 {
		return DefaultPolicy
	}
	return r.Policy
}

func (r *Router) tracker() *health.Tracker {
	r.once.Do(func() {
		if r.Health == nil {
			r.Health = health.New(health.Defaults, nil)
		}
	})
	return r.Health
}

func (r *Router) random() float64 {
	if r.Rand != nil {
		return r.Rand()
	}
	return rand.Float64()
}

func (r *Router) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Candidates lists where a request may go, models in order. An exact model ID
// stays on that model's own deployments (ADR-003); only an alias may cross
// models.
func (r *Router) Candidates(id string) ([]Candidate, string, error) {
	snap := r.Catalog.Load()
	models, policy := []string{id}, "exact"
	if a, ok := snap.Alias(id); ok {
		models, policy = a.Models, "alias:"+a.Policy
	} else if _, ok := snap.Model(id); !ok {
		return nil, "", ErrUnknownModel
	}
	var out []Candidate
	for _, mid := range models {
		m, _ := snap.Model(mid)
		ds := snap.Deployments(mid)
		for _, d := range ds {
			p := r.Providers[d.Provider]
			if p == nil {
				return nil, "", fmt.Errorf("provider not configured: %s", d.Provider)
			}
			out = append(out, Candidate{Model: m, Deployment: d, Provider: p, Price: snap.Prices[d.ID], only: len(ds) == 1})
		}
	}
	if len(out) == 0 {
		return nil, "", ErrUnknownModel
	}
	return out, policy, nil
}

// plan orders each model's deployments: higher priority tiers first, weighted
// random order within a tier (New API's channel pick), then healthy before
// unhealthy. Unhealthy ones are deferred, not removed: their breaker decides.
func (r *Router) plan(cands []Candidate, d *core.RouteDecision) []Candidate {
	var out []Candidate
	for start := 0; start < len(cands); {
		end := start
		for end < len(cands) && cands[end].Model.ID == cands[start].Model.ID {
			end++
		}
		group := append([]Candidate(nil), cands[start:end]...)
		sort.SliceStable(group, func(i, j int) bool { return group[i].Deployment.Priority > group[j].Deployment.Priority })
		for a := 0; a < len(group); {
			b := a
			for b < len(group) && group[b].Deployment.Priority == group[a].Deployment.Priority {
				b++
			}
			r.shuffle(group[a:b])
			a = b
		}
		var healthy, deferred []Candidate
		for _, c := range group {
			if r.tracker().Healthy(c.Deployment.ID) {
				healthy = append(healthy, c)
			} else {
				deferred = append(deferred, c)
				d.Reasons = append(d.Reasons, "deferred_unhealthy:"+c.Deployment.ID)
			}
		}
		out = append(append(out, healthy...), deferred...)
		start = end
	}
	return out
}

// shuffle draws a weighted random order without replacement; weight 0 counts as 1.
func (r *Router) shuffle(tier []Candidate) {
	weight := func(c Candidate) float64 { return float64(max(c.Deployment.Weight, 1)) }
	for i := range tier {
		total := 0.0
		for _, c := range tier[i:] {
			total += weight(c)
		}
		x := r.random() * total
		pick := len(tier) - 1
		for j := i; j < len(tier); j++ {
			if x -= weight(tier[j]); x < 0 {
				pick = j
				break
			}
		}
		tier[i], tier[pick] = tier[pick], tier[i]
	}
}

// route gates every model, ranks the survivors by the alias policy, then
// orders each model's deployments.
func (r *Router) route(req core.ChatRequest, policy string, cands []Candidate, d *core.RouteDecision) ([]Candidate, error) {
	input := estimateInput(req)
	var eligible [][]Candidate
	var why []string
	for _, g := range groups(cands) {
		if reason := gate(g[0], req, input); reason != "" {
			why = append(why, g[0].Model.ID+": "+reason)
			continue
		}
		eligible = append(eligible, g)
	}
	if len(eligible) == 0 {
		return nil, &NoEligibleError{Requested: req.Model, Why: why}
	}
	for _, w := range why {
		d.Reasons = append(d.Reasons, "excluded "+w)
	}
	ranked, err := r.rank(strings.TrimPrefix(policy, "alias:"), eligible, req, input, d)
	if err != nil {
		return nil, err
	}
	var flat []Candidate
	for _, g := range ranked {
		flat = append(flat, g...)
	}
	return r.plan(flat, d), nil
}

// Explain plans a request without calling any provider: the decision a real
// request would start from.
func (r *Router) Explain(req core.ChatRequest) (core.RouteDecision, []Candidate, error) {
	d := core.RouteDecision{Requested: req.Model, Attempts: []core.Attempt{}, Reasons: []string{}}
	cands, policy, err := r.Candidates(req.Model)
	if err != nil {
		return d, nil, err
	}
	d.Policy = policy
	plan, err := r.route(req, policy, cands, &d)
	return d, plan, err
}

func (r *Router) backoff(retry int) time.Duration {
	p := r.policy()
	d := min(p.BackoffInitial<<retry, p.BackoffMax)
	return time.Duration(float64(d) * (0.8 + 0.4*r.random()))
}

// run walks the plan, calling try until one attempt succeeds. Actions follow
// the failure class (ADR-013): transient and timeout retry the same
// deployment once with backoff; rate limits, admission refusals and
// unsupported features move on; quota, key and model failures bench the
// deployment and move on; caller faults, cancellations and unknown errors
// stop. On success the returned ticket still holds its concurrency slot.
func (r *Router) run(ctx context.Context, req core.ChatRequest, stream bool, try func(Candidate, bool) error) (Candidate, *health.Ticket, core.RouteDecision, error) {
	d := core.RouteDecision{Requested: req.Model, Attempts: []core.Attempt{}, Reasons: []string{}}
	cands, policy, err := r.Candidates(req.Model)
	if err != nil {
		return Candidate{}, nil, d, err
	}
	d.Policy = policy
	plan, err := r.route(req, policy, cands, &d)
	if err != nil {
		return Candidate{}, nil, d, err
	}
	p := r.policy()
	var lastErr error
	for i, c := range plan {
		for same := 0; ; same++ {
			if len(d.Attempts) >= p.MaxAttempts {
				d.Reasons = append(d.Reasons, "attempt_budget_exhausted")
				return Candidate{}, nil, d, lastErr
			}
			ticket, err := r.tracker().Acquire(c.Deployment.ID, c.Deployment.MaxConcurrency)
			if err != nil {
				var a *provider.AdmissionError
				errors.As(err, &a)
				d.Reasons = append(d.Reasons, "skipped_"+a.Reason+":"+c.Deployment.ID)
				lastErr = err
				break
			}
			start := time.Now()
			err = try(c, i < len(plan)-1)
			took := time.Since(start)
			class := provider.Class(err)
			ticket.Record(class, took, stream, c.only)
			attempt := core.Attempt{Deployment: c.Deployment.ID, Class: string(class), Millis: took.Milliseconds()}
			var api *provider.APIError
			if errors.As(err, &api) {
				attempt.Status = api.Status
			}
			d.Attempts = append(d.Attempts, attempt)
			if err == nil {
				d.Model, d.Deployment = c.Model.ID, c.Deployment.ID
				return c, ticket, d, nil
			}
			ticket.Release()
			lastErr = err
			wait := time.Duration(-1)
			switch class {
			case provider.ClassCallerFault, provider.ClassCancelled, provider.ClassUnknown:
				d.Reasons = append(d.Reasons, "stop_"+string(class))
				return Candidate{}, nil, d, err
			case provider.ClassTransient, provider.ClassTimeout:
				if same < p.SameRetries {
					wait = r.backoff(same)
				}
			case provider.ClassRateLimit:
				// Another deployment beats waiting; the last one may wait out
				// a Retry-After that fits the budget.
				if i == len(plan)-1 && same < p.SameRetries && api != nil && api.RetryAfter > 0 {
					if api.RetryAfter <= p.MaxWait {
						wait = api.RetryAfter
					} else {
						d.Reasons = append(d.Reasons, "retry_after_exceeds_budget")
					}
				}
			}
			if wait < 0 {
				d.Reasons = append(d.Reasons, "failover_"+string(class)+":"+c.Deployment.ID)
				break
			}
			d.Reasons = append(d.Reasons, "retry_same_"+string(class)+":"+c.Deployment.ID)
			if err := r.sleep(ctx, min(wait, p.MaxWait)); err != nil {
				return Candidate{}, nil, d, err
			}
		}
	}
	return Candidate{}, nil, d, lastErr
}

func (r *Router) Complete(ctx context.Context, req core.ChatRequest) (Candidate, core.Completion, core.RouteDecision, error) {
	var out core.Completion
	c, ticket, d, err := r.run(ctx, req, false, func(c Candidate, _ bool) error {
		var err error
		out, err = c.Provider.Complete(ctx, c.Deployment.UpstreamModel, req)
		return err
	})
	if err != nil {
		return Candidate{}, core.Completion{}, d, err
	}
	ticket.Release()
	return c, out, d, nil
}

// Stream peeks at the first upstream chunk before committing a downstream SSE
// response. Until then a retryable failure may fall back to the next
// candidate. NEVER retry after streaming starts: that could splice responses.
func (r *Router) Stream(ctx context.Context, req core.ChatRequest) (Candidate, provider.Stream, core.RouteDecision, error) {
	var stream *peekStream
	c, ticket, d, err := r.run(ctx, req, true, func(c Candidate, bounded bool) error {
		var err error
		stream, err = r.open(ctx, c, req, bounded)
		return err
	})
	if err != nil {
		return Candidate{}, nil, d, err
	}
	stream.release = ticket.Release // the slot is held until the stream closes
	return c, stream, d, nil
}

// open starts one streaming attempt and peeks at its first chunk. A bounded
// attempt is abandoned when that chunk takes longer than FirstEventTimeout.
func (r *Router) open(ctx context.Context, c Candidate, req core.ChatRequest, bounded bool) (*peekStream, error) {
	ctx, cancel := context.WithCancel(ctx)
	var deadline *time.Timer
	if bounded && r.FirstEventTimeout > 0 {
		deadline = time.AfterFunc(r.FirstEventTimeout, cancel)
	}
	stream, err := c.Provider.Stream(ctx, c.Deployment.UpstreamModel, req)
	// Commit only once something a client would see arrives; usage-only
	// chunks before it are held back with it.
	var pending []core.Chunk
	for err == nil {
		var chunk core.Chunk
		if chunk, err = stream.Recv(); err == io.EOF {
			err = fmt.Errorf("upstream stream ended without content")
		}
		if err == nil {
			pending = append(pending, chunk)
			if chunk.HasContent() {
				break
			}
		}
	}
	if deadline != nil && !deadline.Stop() {
		// Fired: even a chunk that just arrived rides a cancelled request.
		err = fmt.Errorf("no stream event within %s: %w", r.FirstEventTimeout, context.DeadlineExceeded)
	}
	if err != nil {
		if stream != nil {
			stream.Close()
		}
		cancel()
		return nil, err
	}
	return &peekStream{Stream: stream, pending: pending, cancel: cancel}, nil
}

type peekStream struct {
	provider.Stream
	pending []core.Chunk
	cancel  context.CancelFunc
	release func()
}

func (s *peekStream) Close() error {
	err := s.Stream.Close()
	s.cancel()
	if s.release != nil {
		s.release()
	}
	return err
}

func (s *peekStream) Recv() (core.Chunk, error) {
	if len(s.pending) > 0 {
		c := s.pending[0]
		s.pending = s.pending[1:]
		return c, nil
	}
	return s.Stream.Recv()
}

// Headers renders a decision as response headers; reasons never hold prompt text.
func Headers(d core.RouteDecision) map[string]string {
	h := map[string]string{"X-MiniRouter-Attempts": strconv.Itoa(len(d.Attempts))}
	if d.Model != "" {
		h["X-MiniRouter-Resolved-Model"], h["X-MiniRouter-Deployment"] = d.Model, d.Deployment
	}
	reasons := append([]string{d.Policy}, d.Reasons...)
	h["X-MiniRouter-Route-Reason"] = strings.Join(reasons, ",")
	return h
}
