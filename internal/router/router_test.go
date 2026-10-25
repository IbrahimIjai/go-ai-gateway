package router

import (
	"context"
	"errors"
	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/provider"
	"io"
	"testing"
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
