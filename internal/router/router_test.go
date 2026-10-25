package router

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

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
	r := &Router{Models: []core.Model{{ID: "a", Provider: "a"}, {ID: "b", Provider: "b"}}, Providers: map[string]provider.Provider{"a": fake{name: "a", bad: true}, "b": fake{name: "b"}}}
	m, c, err := r.Complete(context.Background(), core.ChatRequest{Model: "router/auto"})
	if err != nil || m.ID != "b" || c.Text != "ok" {
		t.Fatalf("bad fallback %+v %+v %v", m, c, err)
	}
	m, stream, err := r.Stream(context.Background(), core.ChatRequest{Model: "router/auto"})
	if err != nil || m.ID != "b" {
		t.Fatalf("bad stream fallback: %v", err)
	}
	c2, e := stream.Recv()
	if e != nil || c2.Text != "hi" {
		t.Fatalf("peek failed: %+v %v", c2, e)
	}
	_, _, err = r.Complete(context.Background(), core.ChatRequest{Model: "missing"})
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
		r := &Router{Models: []core.Model{{ID: "a", Provider: "a"}, {ID: "b", Provider: "b"}}, Providers: map[string]provider.Provider{"a": broken{tc.err}, "b": fake{name: "b"}}}
		m, _, err := r.Complete(context.Background(), core.ChatRequest{Model: "router/auto"})
		if got := err == nil && m.ID == "b"; got != tc.fallback {
			t.Errorf("%s: want fallback=%v, got model %q err %v", tc.name, tc.fallback, m.ID, err)
		}
		_, _, err = r.Stream(context.Background(), core.ChatRequest{Model: "router/auto"})
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
		Models:    []core.Model{{ID: "a", Provider: "a"}, {ID: "b", Provider: "b"}},
		Providers: map[string]provider.Provider{"a": slow{delay: time.Hour}, "b": fake{name: "b"}}}
	m, st, err := r.Stream(context.Background(), core.ChatRequest{Model: "router/auto"})
	if err != nil || m.ID != "b" {
		t.Fatalf("want fallback to b, got %q %v", m.ID, err)
	}
	st.Close()
}

// The deadline only exists to hand over to a fallback, so the last candidate
// never gets it: a slow answer beats no answer.
func TestLastCandidateIsNeverCutByFirstEventTimeout(t *testing.T) {
	r := &Router{FirstEventTimeout: 5 * time.Millisecond,
		Models:    []core.Model{{ID: "a", Provider: "a"}},
		Providers: map[string]provider.Provider{"a": slow{delay: 50 * time.Millisecond}}}
	m, st, err := r.Stream(context.Background(), core.ChatRequest{Model: "a"})
	if err != nil || m.ID != "a" {
		t.Fatalf("want a, got %q %v", m.ID, err)
	}
	defer st.Close()
	if c, err := st.Recv(); err != nil || c.Text != "late" {
		t.Fatalf("want the late chunk, got %+v %v", c, err)
	}
}
