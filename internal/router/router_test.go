package router

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"example.com/minirouter/internal/catalog"
	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/provider"
)

type fake struct {
	name string
	bad  bool
}

func (p fake) Name() string { return p.name }
func (p fake) Complete(_ context.Context, _ string, _ core.ChatRequest) (core.Completion, error) {
	if p.bad {
		return core.Completion{}, &provider.APIError{Status: 429}
	}
	return core.Completion{Text: "ok"}, nil
}
func (p fake) Stream(_ context.Context, _ string, _ core.ChatRequest) (provider.Stream, error) {
	if p.bad {
		return nil, &provider.APIError{Status: 429}
	}
	return oneStream{}, nil
}

type oneStream struct{}

func (oneStream) Recv() (core.Chunk, error) { return core.Chunk{Text: "hi"}, nil }
func (oneStream) Close() error              { return nil }

var _ io.Closer = oneStream{}

func TestAutoFallback(t *testing.T) {
	r := &Router{Catalog: catalog.Static("a", "b"), Sleep: noWait, Providers: map[string]provider.Provider{"a": fake{name: "a", bad: true}, "b": fake{name: "b"}}}
	m, c, _, err := r.Complete(context.Background(), core.ChatRequest{Model: "router/auto"})
	if err != nil || m.Model.ID != "b" || c.Text != "ok" {
		t.Fatalf("bad fallback %+v %+v %v", m, c, err)
	}
	m, stream, _, err := r.Stream(context.Background(), core.ChatRequest{Model: "router/auto"})
	if err != nil || m.Model.ID != "b" {
		t.Fatalf("bad stream fallback: %v", err)
	}
	c2, e := stream.Recv()
	if e != nil || c2.Text != "hi" {
		t.Fatalf("peek failed: %+v %v", c2, e)
	}
	_, _, _, err = r.Complete(context.Background(), core.ChatRequest{Model: "missing"})
	if !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("unexpected error %v", err)
	}
}

// broken fails every call with err.
type broken struct{ err error }

func (broken) Name() string { return "broken" }
func (p broken) Complete(context.Context, string, core.ChatRequest) (core.Completion, error) {
	return core.Completion{}, p.err
}
func (p broken) Stream(context.Context, string, core.ChatRequest) (provider.Stream, error) {
	return nil, p.err
}

// router/auto hands over to the next model only when the failure belongs to
// the provider; a bad request or a departed client stops at once.
func TestAutoFallbackFollowsFailureClass(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		fallback bool
	}{
		{"network error", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"cut before first event", io.ErrUnexpectedEOF, true},
		{"provider quota", &provider.APIError{Status: 429, Code: "insufficient_quota"}, true},
		{"provider key refused", &provider.APIError{Status: 401}, true},
		{"invalid request", &provider.APIError{Status: 400}, false},
		{"client went away", context.Canceled, false},
	}
	for _, tc := range cases {
		r := &Router{Catalog: catalog.Static("a", "b"), Sleep: noWait, Providers: map[string]provider.Provider{"a": broken{tc.err}, "b": fake{name: "b"}}}
		m, _, _, err := r.Complete(context.Background(), core.ChatRequest{Model: "router/auto"})
		if got := err == nil && m.Model.ID == "b"; got != tc.fallback {
			t.Errorf("%s: want fallback=%v, got model %q err %v", tc.name, tc.fallback, m.Model.ID, err)
		}
		_, _, _, err = r.Stream(context.Background(), core.ChatRequest{Model: "router/auto"})
		if got := err == nil; got != tc.fallback {
			t.Errorf("%s (stream): want fallback=%v, got err %v", tc.name, tc.fallback, err)
		}
	}
}

// slow streams after delay, or fails when its request context ends first.
type slow struct{ delay time.Duration }

func (slow) Name() string { return "slow" }
func (slow) Complete(context.Context, string, core.ChatRequest) (core.Completion, error) {
	return core.Completion{}, errors.New("unused")
}
func (p slow) Stream(ctx context.Context, _ string, _ core.ChatRequest) (provider.Stream, error) {
	return slowStream{ctx: ctx, delay: p.delay}, nil
}

type slowStream struct {
	ctx   context.Context
	delay time.Duration
}

func (s slowStream) Recv() (core.Chunk, error) {
	select {
	case <-time.After(s.delay):
		return core.Chunk{Text: "late"}, nil
	case <-s.ctx.Done():
		return core.Chunk{}, s.ctx.Err()
	}
}
func (slowStream) Close() error { return nil }

func TestFirstEventTimeoutHandsOverToNextCandidate(t *testing.T) {
	r := &Router{FirstEventTimeout: 20 * time.Millisecond,
		Catalog:   catalog.Static("a", "b"),
		Sleep:     noWait,
		Providers: map[string]provider.Provider{"a": slow{delay: time.Hour}, "b": fake{name: "b"}}}
	m, st, _, err := r.Stream(context.Background(), core.ChatRequest{Model: "router/auto"})
	if err != nil || m.Model.ID != "b" {
		t.Fatalf("want fallback to b, got %q %v", m.Model.ID, err)
	}
	st.Close()
}

// The deadline only exists to hand over to a fallback, so the last candidate
// never gets it: a slow answer beats no answer.
func TestLastCandidateIsNeverCutByFirstEventTimeout(t *testing.T) {
	r := &Router{FirstEventTimeout: 5 * time.Millisecond,
		Catalog:   catalog.Static("a"),
		Sleep:     noWait,
		Providers: map[string]provider.Provider{"a": slow{delay: 50 * time.Millisecond}}}
	m, st, _, err := r.Stream(context.Background(), core.ChatRequest{Model: "a"})
	if err != nil || m.Model.ID != "a" {
		t.Fatalf("want a, got %q %v", m.Model.ID, err)
	}
	defer st.Close()
	if c, err := st.Recv(); err != nil || c.Text != "late" {
		t.Fatalf("want the late chunk, got %+v %v", c, err)
	}
}

func twoHostRouter(first, second provider.Provider) *Router {
	c := catalog.Config{
		Models: []catalog.Model{{ID: "m1"}, {ID: "m2"}},
		Deployments: []catalog.Deployment{
			{ID: "m1@host1", Model: "m1", Provider: "host1", UpstreamModel: "x"},
			{ID: "m1@host2", Model: "m1", Provider: "host2", UpstreamModel: "x"},
			{ID: "m2@host3", Model: "m2", Provider: "host3", UpstreamModel: "y"},
		},
	}
	snap, err := catalog.NewSnapshot(c)
	if err != nil {
		panic(err)
	}
	return &Router{Catalog: catalog.NewRegistry(snap), Sleep: noWait, Providers: map[string]provider.Provider{"host1": first, "host2": second, "host3": fake{name: "host3"}}}
}

// An exact model fails over between its own deployments, never to another
// model (ADR-003).
func TestExactModelStaysOnItsDeployments(t *testing.T) {
	r := twoHostRouter(fake{name: "host1", bad: true}, fake{name: "host2"})
	c, _, _, err := r.Complete(context.Background(), core.ChatRequest{Model: "m1"})
	if err != nil || c.Deployment.ID != "m1@host2" || c.Model.ID != "m1" {
		t.Fatalf("want m1 served by its second host, got %+v %v", c.Deployment, err)
	}
	r = twoHostRouter(fake{name: "host1", bad: true}, fake{name: "host2", bad: true})
	if c, _, _, err := r.Complete(context.Background(), core.ChatRequest{Model: "m1"}); err == nil {
		t.Fatalf("both m1 hosts failed, yet %s answered", c.Model.ID)
	}
	if _, _, err := r.Candidates("nope"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("unknown model: %v", err)
	}
}
func noWait(context.Context, time.Duration) error { return nil }
