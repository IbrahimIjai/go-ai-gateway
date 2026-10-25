// Package health tracks each deployment: a circuit breaker (as in
// llm-gateway), timed auto-disable on permanent failures (as New API disables
// channels), a concurrency bulkhead, and rolling latency samples.
package health

import (
	"sync"
	"time"

	"example.com/minirouter/internal/provider"
)

type Config struct {
	FailureThreshold int           // consecutive counted failures that open the breaker
	OpenFor          time.Duration // how long an open breaker rejects before one trial
	DisableFor       time.Duration // how long quota, key or model failures bench a deployment
	Samples          int           // latency samples kept per deployment
}

var Defaults = Config{FailureThreshold: 5, OpenFor: 30 * time.Second, DisableFor: 15 * time.Minute, Samples: 10}

type breaker int

const (
	closed breaker = iota
	open
	halfOpen
)

type state struct {
	breaker       breaker
	failures      int
	openedAt      time.Time
	trial         bool // the one half-open request is in flight
	disabledUntil time.Time
	disabledFor   provider.FailureClass
	inflight      int
	latency, ttft []time.Duration
	minute        int64
	ok, fail      int
}

type Tracker struct {
	cfg   Config
	now   func() time.Time
	mu    sync.Mutex
	state map[string]*state
}

// New returns a tracker; now may be nil for the wall clock.
func New(cfg Config, now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	return &Tracker{cfg: cfg, now: now, state: map[string]*state{}}
}

func (t *Tracker) get(id string) *state {
	s := t.state[id]
	if s == nil {
		s = &state{}
		t.state[id] = s
	}
	return s
}

// Healthy reports whether a deployment would be admitted without a trial:
// planners try healthy ones first and defer the rest.
func (t *Tracker) Healthy(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.get(id)
	return s.breaker == closed && !t.now().Before(s.disabledUntil)
}

// Ticket is one admitted call. Record its outcome once, then Release it.
type Ticket struct {
	t        *Tracker
	id       string
	trial    bool
	released bool
}

// Acquire admits a call, or refuses with an AdmissionError when the
// deployment is disabled, its breaker is open, or limit calls are in flight.
func (t *Tracker) Acquire(id string, limit int) (*Ticket, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, now := t.get(id), t.now()
	refuse := func(reason string) (*Ticket, error) {
		return nil, &provider.AdmissionError{Deployment: id, Reason: reason}
	}
	if now.Before(s.disabledUntil) {
		return refuse("disabled")
	}
	if s.disabledFor != "" { // the bench has expired: earn trust back with one trial
		s.disabledFor, s.breaker = "", halfOpen
	}
	if s.breaker == open {
		if now.Sub(s.openedAt) < t.cfg.OpenFor {
			return refuse("breaker_open")
		}
		s.breaker = halfOpen
	}
	if s.breaker == halfOpen && s.trial {
		return refuse("breaker_open")
	}
	if limit > 0 && s.inflight >= limit {
		return refuse("concurrency_full")
	}
	s.inflight++
	tk := &Ticket{t: t, id: id}
	if s.breaker == halfOpen {
		s.trial, tk.trial = true, true
	}
	return tk, nil
}

// Record files the outcome: class is "" on success. A rate limit on a model's
// only deployment does not count against it (LiteLLM's cooldown rule): there
// is nowhere better to send the traffic.
func (k *Ticket) Record(class provider.FailureClass, took time.Duration, stream, onlyDeployment bool) {
	t := k.t
	t.mu.Lock()
	defer t.mu.Unlock()
	s, now := t.get(k.id), t.now()
	if k.trial {
		s.trial = false
	}
	minute := now.Unix() / 60
	if minute != s.minute {
		s.minute, s.ok, s.fail = minute, 0, 0
	}
	switch class {
	case "":
		s.ok++
		s.failures, s.breaker = 0, closed
		t.sample(s, took, stream)
	case provider.ClassQuota, provider.ClassCredential, provider.ClassModelGone:
		s.fail++
		s.disabledUntil, s.disabledFor = now.Add(t.cfg.DisableFor), class
	case provider.ClassTransient, provider.ClassTimeout, provider.ClassRateLimit:
		s.fail++
		if class == provider.ClassTimeout {
			t.sample(s, took, stream) // a timeout is a very slow answer, not no data
		}
		if class == provider.ClassRateLimit && onlyDeployment {
			return
		}
		if s.failures++; s.breaker == halfOpen || s.failures >= t.cfg.FailureThreshold {
			s.breaker, s.openedAt, s.failures = open, now, 0
		}
	}
	// Other classes (caller fault, cancellation, unsupported) say nothing
	// about the deployment's health.
}

func (t *Tracker) sample(s *state, took time.Duration, stream bool) {
	ring := &s.latency
	if stream {
		ring = &s.ttft
	}
	*ring = append(*ring, took)
	if len(*ring) > t.cfg.Samples {
		*ring = (*ring)[1:]
	}
}

// Release frees the concurrency slot; it is safe to call more than once.
func (k *Ticket) Release() {
	t := k.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if !k.released {
		k.released = true
		t.get(k.id).inflight--
	}
}

// Stats is a deployment's current health, for decisions and operators.
type Stats struct {
	Breaker       string                `json:"breaker"` // closed | open | half_open
	DisabledUntil *time.Time            `json:"disabled_until,omitempty"`
	DisabledFor   provider.FailureClass `json:"disabled_for,omitempty"`
	InFlight      int                   `json:"in_flight"`
	Latency       []time.Duration       `json:"latency_samples"`
	TTFT          []time.Duration       `json:"ttft_samples"`
	OKThisMinute  int                   `json:"ok_this_minute"`
	FailThisMin   int                   `json:"failures_this_minute"`
}

func (t *Tracker) Stats(id string) Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.get(id)
	st := Stats{Breaker: [...]string{"closed", "open", "half_open"}[s.breaker], InFlight: s.inflight,
		Latency: append([]time.Duration(nil), s.latency...), TTFT: append([]time.Duration(nil), s.ttft...)}
	if t.now().Before(s.disabledUntil) {
		until := s.disabledUntil
		st.DisabledUntil, st.DisabledFor = &until, s.disabledFor
	}
	if s.minute == t.now().Unix()/60 {
		st.OKThisMinute, st.FailThisMin = s.ok, s.fail
	}
	return st
}

// Enable lifts a disable and resets the breaker, for operators.
func (t *Tracker) Enable(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.get(id)
	s.disabledUntil, s.disabledFor, s.breaker, s.failures = time.Time{}, "", closed, 0
}
