package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"example.com/minirouter/internal/billing"
	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/router"
	"example.com/minirouter/internal/store"
)

// Credits is the prepaid ledger (the PostgreSQL store). When set, every
// request from a project holds its maximum possible cost first (ADR-014).
type Credits interface {
	Reserve(ctx context.Context, projectID, requestID string, micros int64, ttl time.Duration) error
	Settle(ctx context.Context, requestID string, cost int64) error
	Release(ctx context.Context, requestID string) error
	Hold(ctx context.Context, requestID, note string) error
	Claim(ctx context.Context, projectID, key string, body []byte, requestID string) (string, bool, error)
}

// holdTTL bounds how long a hold may stay active; the worker resolves holds
// left behind by a crash once it passes.
const holdTTL = 15 * time.Minute

// MaxInputTokens is an upper bound on input tokens without a tokenizer:
// byte-level BPE and byte-fallback tokenizers emit at most one token per
// UTF-8 byte, plus a generous allowance for message and tool framing.
func MaxInputTokens(req core.ChatRequest) int64 {
	n := int64(0)
	for _, m := range req.Messages {
		n += 16 + int64(len(m.Text()))
		for _, c := range m.ToolCalls {
			n += 16 + int64(len(c.ID)+len(c.Name)+len(c.Arguments))
		}
	}
	for _, t := range req.Tools {
		n += 16 + int64(len(t.Name)+len(t.Description)+len(t.Parameters))
	}
	return n
}

// holdFor prices the worst case over every deployment the request may use.
// A deployment without a verified price makes the bound unknowable, so the
// request is refused rather than run on credit it might exceed.
func (s *Server) holdFor(req core.ChatRequest) (int64, error) {
	_, plan, err := s.Router.Explain(req)
	if err != nil {
		return 0, err
	}
	worst := core.Usage{InputTokens: MaxInputTokens(req), OutputTokens: int64(req.MaxOutputTokens)}
	hold := int64(0)
	for _, c := range plan {
		cost, known := billing.CostMicros(worst, c.Price)
		if !known {
			return 0, fmt.Errorf("deployment %s has no verified price; credit-backed requests need one", c.Deployment.ID)
		}
		hold = max(hold, cost)
	}
	return max(hold, 1), nil // a hold is never zero, so it is always recorded
}

// reserve claims the idempotency key and holds credit, answering itself when
// the request may not proceed. It reports whether a hold was taken.
func (s *Server) reserve(w http.ResponseWriter, r *http.Request, req core.ChatRequest, id string) (held, ok bool) {
	p := principal(r)
	if s.Credits == nil || p.ProjectID == "" {
		return false, true
	}
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		body, _ := json.Marshal(req)
		original, same, err := s.Credits.Claim(r.Context(), p.ProjectID, key, body, id)
		if errors.Is(err, store.ErrDuplicate) {
			msg := "Idempotency-Key was already used for request " + original
			if !same {
				msg += " with a different body"
			}
			jsonError(w, http.StatusConflict, "idempotency_conflict", msg)
			return false, false
		}
		if err != nil {
			s.storageError(w, err)
			return false, false
		}
	}
	hold, err := s.holdFor(req)
	if err != nil {
		var none *router.NoEligibleError
		if errors.As(err, &none) || errors.Is(err, router.ErrUnknownModel) {
			s.fail(w, err)
		} else {
			jsonError(w, http.StatusBadRequest, "unpriced_model", err.Error())
		}
		return false, false
	}
	if err := s.Credits.Reserve(r.Context(), p.ProjectID, id, hold, holdTTL); err != nil {
		if errors.Is(err, store.ErrInsufficientFunds) {
			jsonError(w, http.StatusPaymentRequired, "insufficient_credit", fmt.Sprintf("this request may cost up to %s USD; add credit or lower max_completion_tokens", billing.FormatUSD(hold)))
		} else {
			s.storageError(w, err)
		}
		return false, false
	}
	return true, true
}

// settleCredits resolves a request's hold exactly once: failures are
// released, known costs settled, unknown costs kept for review (ADR-006).
func (s *Server) settleCredits(r *http.Request, id string, c router.Candidate, usage *core.Usage, failed bool, err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	var cost int64
	known := false
	if usage != nil {
		cost, known = billing.CostMicros(*usage, c.Price)
	}
	var serr error
	switch {
	case err != nil:
		serr = s.Credits.Release(ctx, id)
	case failed || usage == nil:
		serr = s.Credits.Hold(ctx, id, "usage unknown: stream cut or no usage reported")
	case !known:
		serr = s.Credits.Hold(ctx, id, "usage known but not priceable")
	default:
		serr = s.Credits.Settle(ctx, id, cost)
	}
	if serr != nil && s.Log != nil {
		// The hold stays active; the worker resolves it after it expires.
		s.Log.Error("credit not resolved", "id", id, "error", serr)
	}
}

func (s *Server) storageError(w http.ResponseWriter, err error) {
	if s.Log != nil {
		s.Log.Error("storage error", "error", err)
	}
	jsonError(w, http.StatusServiceUnavailable, "storage_unavailable", "billing storage is unavailable; try again later")
}
