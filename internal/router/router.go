// Package router resolves public model IDs, dispatches requests and retries
// safely before emitting any downstream stream bytes.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/provider"
)

var ErrUnknownModel = errors.New("unknown or unavailable model")

type Router struct {
	Models    []core.Model
	Providers map[string]provider.Provider
	// FirstEventTimeout bounds the wait for a stream's first event on every
	// attempt that still has a fallback behind it. The last attempt always
	// waits, so a slow answer beats no answer (Bifrost's rule). Zero disables.
	FirstEventTimeout time.Duration
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
// response. Until then a retryable failure may fall back to the next
// candidate. NEVER retry after streaming starts: that could splice responses.
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
		last := i == len(candidates)-1
		stream, callErr := r.open(ctx, p, m, req, !last)
		if callErr == nil {
			return m, stream, nil
		}
		if last || !provider.Retryable(callErr) {
			return core.Model{}, nil, callErr
		}
	}
	return core.Model{}, nil, ErrUnknownModel
}

// open starts one streaming attempt and peeks at its first chunk. A bounded
// attempt is abandoned when that chunk takes longer than FirstEventTimeout.
func (r *Router) open(ctx context.Context, p provider.Provider, m core.Model, req core.ChatRequest, bounded bool) (provider.Stream, error) {
	ctx, cancel := context.WithCancel(ctx)
	var deadline *time.Timer
	if bounded && r.FirstEventTimeout > 0 {
		deadline = time.AfterFunc(r.FirstEventTimeout, cancel)
	}
	stream, err := p.Stream(ctx, m.Upstream, req)
	var first core.Chunk
	if err == nil {
		if first, err = stream.Recv(); err == io.EOF {
			err = fmt.Errorf("upstream stream ended without content")
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
	return &peekStream{Stream: stream, first: &first, cancel: cancel}, nil
}

type peekStream struct {
	provider.Stream
	first  *core.Chunk
	cancel context.CancelFunc
}

func (s *peekStream) Close() error {
	err := s.Stream.Close()
	s.cancel()
	return err
}

func (s *peekStream) Recv() (core.Chunk, error) {
	if s.first != nil {
		c := *s.first
		s.first = nil
		return c, nil
	}
	return s.Stream.Recv()
}
