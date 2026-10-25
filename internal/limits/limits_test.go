package limits

import (
	"errors"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func limited(err error) string {
	var e *Exceeded
	if errors.As(err, &e) {
		return e.Limit
	}
	return ""
}

func TestAllOrNothing(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 25, 12, 0, 10, 0, time.UTC)}
	l := New(c.now)
	lim := Limits{RPM: 10, TPM: 100}
	g, err := l.Acquire("k", lim, 80)
	if err != nil {
		t.Fatal(err)
	}
	g.Settle(0, false)
	_, err = l.Acquire("k", lim, 50)
	var e *Exceeded
	if !errors.As(err, &e) || e.Limit != "tpm" || e.RetryAfter != 50*time.Second {
		t.Fatalf("want tpm refusal until the next minute, got %v", err)
	}
	if l.keys["k"].requests != 1 {
		t.Fatal("a refused request must not consume the request limit")
	}
	c.t = c.t.Add(time.Minute)
	if _, err := l.Acquire("k", lim, 50); err != nil {
		t.Fatalf("a new minute resets the window: %v", err)
	}
}

func TestSettleCorrectsTheEstimate(t *testing.T) {
	l := New(nil)
	lim := Limits{TPM: 100}
	g, _ := l.Acquire("k", lim, 90)
	g.Settle(10, true)
	g.Settle(10, true) // idempotent
	if _, err := l.Acquire("k", lim, 85); err != nil {
		t.Fatalf("actual usage (10) replaces the estimate (90): %v", err)
	}
}

func TestConcurrencyAndRPM(t *testing.T) {
	l := New(nil)
	g, _ := l.Acquire("k", Limits{Concurrency: 1}, 0)
	if _, err := l.Acquire("k", Limits{Concurrency: 1}, 0); limited(err) != "concurrency" {
		t.Fatalf("want concurrency refusal, got %v", err)
	}
	g.Settle(0, false)
	if _, err := l.Acquire("k", Limits{Concurrency: 1}, 0); err != nil {
		t.Fatal("settling frees the slot")
	}
	for range 2 {
		l.Acquire("r", Limits{RPM: 2}, 0)
	}
	if _, err := l.Acquire("r", Limits{RPM: 2}, 0); limited(err) != "rpm" {
		t.Fatalf("want rpm refusal, got %v", err)
	}
	if g, err := l.Acquire("free", Limits{}, 1_000_000); err != nil || !g.unlimit {
		t.Fatal("no limits means no tracking")
	}
}
