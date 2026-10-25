// Package limits enforces per-key request, token and concurrency limits. Like
// LiteLLM's v3 limiter it checks every descriptor and charges all of them or
// none, so a request refused on one limit never consumes another. This is the
// in-process version; a shared store (Valkey) can implement the same calls
// when several gateway instances run.
package limits

import (
	"fmt"
	"sync"
	"time"
)

// Limits for one key; zero means unlimited.
type Limits struct {
	RPM         int // requests per minute
	TPM         int // estimated tokens per minute, corrected after the call
	Concurrency int // requests in flight
}

// Exceeded says which limit refused a request and when to retry.
type Exceeded struct {
	Limit      string // rpm | tpm | concurrency
	RetryAfter time.Duration
}

func (e *Exceeded) Error() string {
	return fmt.Sprintf("%s limit reached; retry in %s", e.Limit, e.RetryAfter.Round(time.Second))
}

type window struct {
	start           int64 // unix minute
	requests, token int
	inflight        int
}

type Limiter struct {
	mu   sync.Mutex
	now  func() time.Time
	keys map[string]*window
}

// New returns a limiter; now may be nil for the wall clock.
func New(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, keys: map[string]*window{}}
}

// Grant is an admitted request. Settle it once with the actual token count;
// it releases the concurrency slot.
type Grant struct {
	l        *Limiter
	key      string
	minute   int64
	tokens   int
	unlimit  bool
	released bool
}

// Acquire admits one request estimated at tokens, or refuses without
// charging anything.
func (l *Limiter) Acquire(key string, lim Limits, tokens int) (*Grant, error) {
	if lim == (Limits{}) {
		return &Grant{unlimit: true}, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	minute := now.Unix() / 60
	w := l.keys[key]
	if w == nil {
		w = &window{start: minute}
		l.keys[key] = w
	}
	if w.start != minute {
		w.start, w.requests, w.token = minute, 0, 0
	}
	untilNext := time.Unix((minute+1)*60, 0).Sub(now)
	switch {
	case lim.Concurrency > 0 && w.inflight >= lim.Concurrency:
		return nil, &Exceeded{Limit: "concurrency", RetryAfter: time.Second}
	case lim.RPM > 0 && w.requests+1 > lim.RPM:
		return nil, &Exceeded{Limit: "rpm", RetryAfter: untilNext}
	case lim.TPM > 0 && w.token+tokens > lim.TPM:
		return nil, &Exceeded{Limit: "tpm", RetryAfter: untilNext}
	}
	w.requests++
	w.token += tokens
	w.inflight++
	return &Grant{l: l, key: key, minute: minute, tokens: tokens}, nil
}

// Settle replaces the estimate with the actual token count (when known) and
// frees the concurrency slot. It is safe to call more than once.
func (g *Grant) Settle(actual int64, known bool) {
	if g.unlimit {
		return
	}
	l := g.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if g.released {
		return
	}
	g.released = true
	w := l.keys[g.key]
	w.inflight--
	if known && w.start == g.minute {
		w.token = max(0, w.token+int(actual)-g.tokens)
	}
}
