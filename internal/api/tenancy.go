package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"example.com/minirouter/internal/billing"
	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/limits"
	"example.com/minirouter/internal/provider"
	"example.com/minirouter/internal/router"
	"example.com/minirouter/internal/store"
)

// Principal is the authenticated caller.
type Principal struct {
	KeyID, ProjectID string
	Models           []string // allowed model or alias IDs; empty: any
	Limits           limits.Limits
}

// Keys resolves per-project API keys (the PostgreSQL store).
type Keys interface {
	Authenticate(ctx context.Context, raw string) (store.Key, error)
}

// Recorder persists request records; nil keeps them in memory only.
type Recorder interface {
	SaveRequest(ctx context.Context, r store.Record) error
}

type principalKey struct{}

func bearer(r *http.Request) (string, bool) {
	// The scheme is case-insensitive (RFC 7235); a bare key is not a bearer token.
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	return token, strings.EqualFold(scheme, "Bearer") && token != ""
}

// auth admits a database key when Keys is set, else the single static key.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r)
		var p Principal
		switch {
		case ok && s.Keys != nil:
			k, err := s.Keys.Authenticate(r.Context(), token)
			if err != nil {
				if !errors.Is(err, store.ErrInvalidKey) && s.Log != nil {
					s.Log.Error("key lookup failed", "error", err)
				}
				ok = false
				break
			}
			p = Principal{KeyID: k.ID, ProjectID: k.ProjectID, Models: k.Models, Limits: k.Limits}
		case ok:
			ok = s.Key != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.Key)) == 1
		}
		if !ok {
			jsonError(w, http.StatusUnauthorized, "invalid_api_key", "Missing or invalid bearer key")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	}
}

func principal(r *http.Request) Principal {
	p, _ := r.Context().Value(principalKey{}).(Principal)
	return p
}

// admit enforces the key's model allowlist and rate limits, answering itself
// when the request may not proceed.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, req core.ChatRequest) (*limits.Grant, bool) {
	p := principal(r)
	if len(p.Models) > 0 && !slices.Contains(p.Models, req.Model) {
		jsonError(w, http.StatusForbidden, "model_not_allowed", "this key may not use "+req.Model)
		return nil, false
	}
	if s.Limiter == nil || p.KeyID == "" {
		return nil, true // nothing to limit
	}
	g, err := s.Limiter.Acquire(p.KeyID, p.Limits, router.EstimateInput(req)+req.MaxOutputTokens)
	var exceeded *limits.Exceeded
	if errors.As(err, &exceeded) {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(exceeded.RetryAfter.Seconds()+0.999))))
		jsonError(w, http.StatusTooManyRequests, "rate_limit_exceeded", exceeded.Error())
		return nil, false
	}
	return g, true
}

// finish journals a request in memory and, when configured, durably. A cut
// stream or missing usage is recorded as unavailable, never as zero; a
// failure is recorded with its class.
func (s *Server) finish(r *http.Request, id string, req core.ChatRequest, c router.Candidate, route core.RouteDecision, usage *core.Usage, failed bool, err error, held bool) {
	if held {
		defer s.settleCredits(r, id, c, usage, failed, err) // after the record, which the worker reads
	}
	rec := store.Record{ID: id, Requested: req.Model, Resolved: c.Model.ID, Deployment: c.Deployment.ID, Stream: req.Stream, Route: route, Status: "ok", Usage: usage}
	switch {
	case err != nil:
		rec.Status, rec.ErrorClass, rec.Usage = "failed", string(provider.Class(err)), nil
	case failed || usage == nil:
		rec.Status, rec.Usage = "usage_unavailable", nil
	}
	if err == nil {
		s.Store.Add(core.UsageRecord{ID: id, Model: c.Model.ID, Provider: c.Deployment.Provider, Deployment: c.Deployment.ID, Status: rec.Status, Usage: rec.Usage, Route: &route}, c.Price)
	}
	if rec.Usage != nil {
		if cost, known := billing.CostMicros(*rec.Usage, c.Price); known {
			rec.CostMicros = &cost
		}
	}
	if s.Records == nil {
		return
	}
	p := principal(r)
	rec.ProjectID, rec.KeyID = p.ProjectID, p.KeyID
	// The client may be gone; the record must still be written.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := s.Records.SaveRequest(ctx, rec); err != nil && s.Log != nil {
		s.Log.Error("request record not saved", "id", id, "error", err)
	}
}
