package router

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/minirouter/internal/catalog"
	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/health"
	"example.com/minirouter/internal/provider"
)

// script fails its first len(errs) calls with those errors, then answers.
type script struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (*script) Name() string { return "script" }
func (s *script) next() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		return err
	}
	return nil
}
func (s *script) Complete(context.Context, string, core.ChatRequest) (core.Completion, error) {
	if err := s.next(); err != nil {
		return core.Completion{}, err
	}
	return core.Completion{Text: "ok"}, nil
}
func (s *script) Stream(context.Context, string, core.ChatRequest) (provider.Stream, error) {
	if err := s.next(); err != nil {
		return nil, err
	}
	return oneStream{}, nil
}

type deployment struct {
	id       string
	priority int
	weight   int
	limit    int
	p        provider.Provider
}

// pool serves model "m" from the given deployments.
func pool(ds ...deployment) *Router {
	c := catalog.Config{Models: []catalog.Model{{ID: "m"}}}
	providers := map[string]provider.Provider{}
	for _, d := range ds {
		c.Deployments = append(c.Deployments, catalog.Deployment{ID: d.id, Model: "m", Provider: d.id, UpstreamModel: "x", Priority: d.priority, Weight: d.weight, MaxConcurrency: d.limit})
		providers[d.id] = d.p
	}
	snap, err := catalog.NewSnapshot(c)
	if err != nil {
		panic(err)
	}
	return &Router{Catalog: catalog.NewRegistry(snap), Providers: providers,
		Health: health.New(health.Defaults, nil), Rand: func() float64 { return 0 },
		Sleep: func(context.Context, time.Duration) error { return nil }}
}

func complete(t *testing.T, r *Router) (Candidate, core.RouteDecision, error) {
	t.Helper()
	c, _, d, err := r.Complete(context.Background(), core.ChatRequest{Model: "m"})
	return c, d, err
}

func TestPriorityTiersComeFirst(t *testing.T) {
	r := pool(deployment{id: "low", priority: 0, p: &script{}}, deployment{id: "high", priority: 5, p: &script{}})
	if c, _, err := complete(t, r); err != nil || c.Deployment.ID != "high" {
		t.Fatalf("want the higher tier, got %s %v", c.Deployment.ID, err)
	}
}

func TestWeightedPickWithinATier(t *testing.T) {
	r := pool(deployment{id: "a", weight: 1, p: &script{}}, deployment{id: "b", weight: 3, p: &script{}})
	counts := map[string]int{}
	for _, x := range []float64{0.1, 0.3, 0.5, 0.9} { // draws over total weight 4
		r.Rand = func() float64 { return x }
		c, _, _ := complete(t, r)
		counts[c.Deployment.ID]++
	}
	if counts["a"] != 1 || counts["b"] != 3 {
		t.Fatalf("want picks proportional to weight 1:3, got %v", counts)
	}
}

func TestFailureClassDrivesTheNextStep(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantCalls [2]int // calls to first and second deployment
		served    string
		reason    string
	}{
		{"transient retries once on the same deployment", &provider.APIError{Status: 503}, [2]int{2, 0}, "first", "retry_same_transient:first"},
		{"rate limit moves on at once", &provider.APIError{Status: 429}, [2]int{1, 1}, "second", "failover_rate_limit:first"},
		{"quota benches and moves on", &provider.APIError{Status: 429, Code: "insufficient_quota"}, [2]int{1, 1}, "second", "failover_quota:first"},
		{"unsupported moves on", &provider.UnsupportedError{Provider: "x", Feature: "y"}, [2]int{1, 1}, "second", "failover_unsupported:first"},
		{"caller fault stops", &provider.APIError{Status: 400}, [2]int{1, 0}, "", "stop_caller_fault"},
	}
	for _, tc := range cases {
		first, second := &script{errs: []error{tc.err}}, &script{}
		r := pool(deployment{id: "first", priority: 1, p: first}, deployment{id: "second", p: second})
		c, d, err := complete(t, r)
		if first.calls != tc.wantCalls[0] || second.calls != tc.wantCalls[1] || c.Deployment.ID != tc.served || !strings.Contains(strings.Join(d.Reasons, ","), tc.reason) {
			t.Errorf("%s: calls %d/%d served %q err %v reasons %v", tc.name, first.calls, second.calls, c.Deployment.ID, err, d.Reasons)
		}
	}
}

func TestQuotaBenchesTheDeployment(t *testing.T) {
	first := &script{errs: []error{&provider.APIError{Status: 402}}}
	r := pool(deployment{id: "first", priority: 1, p: first}, deployment{id: "second", p: &script{}})
	complete(t, r)
	_, d, _ := complete(t, r)
	if first.calls != 1 || !strings.Contains(strings.Join(d.Reasons, ","), "deferred_unhealthy:first") {
		t.Fatalf("a disabled deployment is deferred and refused: calls %d reasons %v", first.calls, d.Reasons)
	}
}

func TestRetryAfterIsHonouredOnlyWithinBudget(t *testing.T) {
	var waited []time.Duration
	only := &script{errs: []error{&provider.APIError{Status: 429, RetryAfter: 2 * time.Second}}}
	r := pool(deployment{id: "only", p: only})
	r.Sleep = func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }
	if _, _, err := complete(t, r); err != nil || only.calls != 2 || len(waited) != 1 || waited[0] != 2*time.Second {
		t.Fatalf("the last deployment waits out a short Retry-After: calls %d waited %v err %v", only.calls, waited, err)
	}
	only = &script{errs: []error{&provider.APIError{Status: 429, RetryAfter: time.Hour}}}
	r = pool(deployment{id: "only", p: only})
	_, d, err := complete(t, r)
	if err == nil || only.calls != 1 || !strings.Contains(strings.Join(d.Reasons, ","), "retry_after_exceeds_budget") {
		t.Fatalf("an hour-long Retry-After is not waited: calls %d err %v reasons %v", only.calls, err, d.Reasons)
	}
}

func TestAttemptBudget(t *testing.T) {
	var ds []deployment
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		ds = append(ds, deployment{id: id, p: &script{errs: []error{&provider.APIError{Status: 429}}}})
	}
	r := pool(ds...)
	_, d, err := complete(t, r)
	if err == nil || len(d.Attempts) != DefaultPolicy.MaxAttempts || d.Reasons[len(d.Reasons)-1] != "attempt_budget_exhausted" {
		t.Fatalf("want %d attempts then stop: %+v %v", DefaultPolicy.MaxAttempts, d, err)
	}
}

func TestBulkheadFailsOverWithoutCalling(t *testing.T) {
	busy := &script{}
	r := pool(deployment{id: "busy", priority: 1, limit: 1, p: busy}, deployment{id: "spare", p: &script{}})
	_, st, _, err := r.Stream(context.Background(), core.ChatRequest{Model: "m"}) // holds busy's only slot
	if err != nil {
		t.Fatal(err)
	}
	c, d, err := complete(t, r)
	if err != nil || c.Deployment.ID != "spare" || busy.calls != 1 || !strings.Contains(strings.Join(d.Reasons, ","), "skipped_concurrency_full:busy") {
		t.Fatalf("a full bulkhead hands over without an upstream call: %s %v %v", c.Deployment.ID, d.Reasons, err)
	}
	st.Close()
	if c, _, _ := complete(t, r); c.Deployment.ID != "busy" {
		t.Fatal("closing the stream frees the slot")
	}
}

func TestBreakerOpensAndRefusesWithoutCalling(t *testing.T) {
	var errs []error
	for range health.Defaults.FailureThreshold {
		errs = append(errs, &provider.APIError{Status: 500})
	}
	flaky := &script{errs: errs}
	r := pool(deployment{id: "flaky", p: flaky})
	for range health.Defaults.FailureThreshold {
		complete(t, r)
	}
	calls := flaky.calls
	_, d, err := complete(t, r)
	if provider.Class(err) != provider.ClassAdmission || flaky.calls != calls || !strings.Contains(strings.Join(d.Reasons, ","), "skipped_breaker_open:flaky") {
		t.Fatalf("an open breaker refuses at once: class %s calls %d→%d reasons %v", provider.Class(err), calls, flaky.calls, d.Reasons)
	}
}

func TestDecisionHeaders(t *testing.T) {
	r := pool(deployment{id: "first", priority: 1, p: &script{errs: []error{&provider.APIError{Status: 429}}}}, deployment{id: "second", p: &script{}})
	_, d, _ := complete(t, r)
	h := Headers(d)
	if h["X-MiniRouter-Attempts"] != "2" || h["X-MiniRouter-Deployment"] != "second" || h["X-MiniRouter-Route-Reason"] != "exact,failover_rate_limit:first" {
		t.Fatalf("bad headers: %v", h)
	}
}
