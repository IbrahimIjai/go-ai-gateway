// Package router resolves public model IDs, dispatches requests and retries
// safely before emitting any downstream stream bytes.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/provider"
)

var ErrUnknownModel = errors.New("unknown or unavailable model")

type Router struct {
	Models    []core.Model
	Providers map[string]provider.Provider
}

func (r *Router) List() []core.Model {
	out := append([]core.Model(nil), r.Models...)
	out = append(out, core.Model{ID: "router/auto"})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (r *Router) Candidates(id string) ([]core.Model, error) {
	var out []core.Model
	if id == "router/auto" {
		for _, m := range r.Models {
			if m.Provider != "demo" {
				out = append(out, m)
			}
		}
		if len(out) == 0 {
			for _, m := range r.Models {
				if m.Provider == "demo" {
					out = append(out, m)
				}
			}
		}
		// For the learning MVP, try providers in configured order. Only
		// sort when all prices are known; otherwise price-based selection
		// would silently treat unknown rates as free.
		priced := len(out) > 0
		for _, m := range out {
			if m.Price == nil {
				priced = false
			}
		}
		if priced {
			sort.SliceStable(out, func(i, j int) bool {
				a := out[i].Price.InputMicrosPerMillion + out[i].Price.OutputMicrosPerMillion
				b := out[j].Price.InputMicrosPerMillion + out[j].Price.OutputMicrosPerMillion
				return a < b
			})
		}
	} else {
		for _, m := range r.Models {
			if m.ID == id {
				out = append(out, m)
			}
		}
	}
	if len(out) == 0 {
		return nil, ErrUnknownModel
	}
	return out, nil
}
func (r *Router) Complete(ctx context.Context, req core.ChatRequest) (core.Model, core.Completion, error) {
	candidates, err := r.Candidates(req.Model)
	if err != nil {
		return core.Model{}, core.Completion{}, err
	}
	for i, m := range candidates {
		p := r.Providers[m.Provider]
		if p == nil {
			return core.Model{}, core.Completion{}, fmt.Errorf("provider not configured: %s", m.Provider)
		}
		out, callErr := p.Complete(ctx, m.Upstream, req)
		if callErr == nil {
			return m, out, nil
		}
		if i == len(candidates)-1 || !provider.Retryable(callErr) {
			return core.Model{}, core.Completion{}, callErr
		}
	}
	return core.Model{}, core.Completion{}, ErrUnknownModel
}

// Stream peeks at the first upstream chunk before committing a downstream SSE
// response. If the HTTP request fails with a retryable status, fallback is
// possible. NEVER retry after streaming starts: that could splice responses.
func (r *Router) Stream(ctx context.Context, req core.ChatRequest) (core.Model, provider.Stream, error) {
	candidates, err := r.Candidates(req.Model)
	if err != nil {
		return core.Model{}, nil, err
	}
	for i, m := range candidates {
		p := r.Providers[m.Provider]
		if p == nil {
			return core.Model{}, nil, fmt.Errorf("provider not configured: %s", m.Provider)
		}
		stream, callErr := p.Stream(ctx, m.Upstream, req)
		if callErr == nil {
			first, readErr := stream.Recv()
			if readErr == nil {
				return m, &peekStream{Stream: stream, first: &first}, nil
			}
			stream.Close()
			if readErr == io.EOF {
				return core.Model{}, nil, fmt.Errorf("upstream stream ended without content")
			}
			callErr = readErr
		}
		if i == len(candidates)-1 || !provider.Retryable(callErr) {
			return core.Model{}, nil, callErr
		}
	}
	return core.Model{}, nil, ErrUnknownModel
}

type peekStream struct {
	provider.Stream
	first *core.Chunk
}

func (s *peekStream) Recv() (core.Chunk, error) {
	if s.first != nil {
		c := *s.first
		s.first = nil
		return c, nil
	}
	return s.Stream.Recv()
}
