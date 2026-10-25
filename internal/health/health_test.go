package health

import (
	"errors"
	"testing"
	"time"

	"example.com/minirouter/internal/provider"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTracker() (*Tracker, *clock) {
	c := &clock{t: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	return New(Config{FailureThreshold: 2, OpenFor: 30 * time.Second, DisableFor: time.Hour, Samples: 3}, c.now), c
}

func call(t *testing.T, tr *Tracker, class provider.FailureClass) error {
	t.Helper()
	k, err := tr.Acquire("d", 0)
	if err != nil {
		return err
	}
	k.Record(class, time.Second, false, false)
	k.Release()
	return nil
}

func refusedFor(err error) string {
	var a *provider.AdmissionError
	if errors.As(err, &a) {
		return a.Reason
	}
	return ""
}

func TestBreakerOpensThenAdmitsOneTrial(t *testing.T) {
	tr, c := newTracker()
	call(t, tr, provider.ClassTransient)
	call(t, tr, provider.ClassTransient)
	if got := refusedFor(call(t, tr, "")); got != "breaker_open" {
		t.Fatalf("want an open breaker after 2 failures, got %q", got)
	}
	c.t = c.t.Add(31 * time.Second)
	trial, err := tr.Acquire("d", 0)
	if err != nil {
		t.Fatalf("after OpenFor one trial is admitted: %v", err)
	}
	if _, err := tr.Acquire("d", 0); refusedFor(err) != "breaker_open" {
		t.Fatal("only one trial may be in flight")
	}
	trial.Record(provider.ClassTimeout, time.Second, false, false)
	trial.Release()
	if refusedFor(call(t, tr, "")) != "breaker_open" {
		t.Fatal("a failed trial reopens the breaker")
	}
	c.t = c.t.Add(31 * time.Second)
	if err := call(t, tr, ""); err != nil || !tr.Healthy("d") {
		t.Fatalf("a successful trial closes the breaker: %v", err)
	}
}

func TestNeutralFailuresDoNotTrip(t *testing.T) {
	tr, _ := newTracker()
	for range 5 {
		call(t, tr, provider.ClassCallerFault)
		call(t, tr, provider.ClassCancelled)
	}
	if !tr.Healthy("d") {
		t.Fatal("caller faults and cancellations say nothing about the deployment")
	}
	k, _ := tr.Acquire("d", 0)
	k.Record(provider.ClassRateLimit, 0, false, true)
	k, _ = tr.Acquire("d", 0)
	k.Record(provider.ClassRateLimit, 0, false, true)
	if !tr.Healthy("d") {
		t.Fatal("rate limits on a model's only deployment must not open its breaker")
	}
}

func TestQuotaDisablesUntilExpiry(t *testing.T) {
	tr, c := newTracker()
	call(t, tr, provider.ClassQuota)
	if got := refusedFor(call(t, tr, "")); got != "disabled" || tr.Stats("d").DisabledFor != provider.ClassQuota {
		t.Fatalf("want disabled for quota, got %q", got)
	}
	c.t = c.t.Add(time.Hour)
	if err := call(t, tr, ""); err != nil || !tr.Healthy("d") {
		t.Fatalf("after DisableFor a trial succeeds and restores the deployment: %v", err)
	}
	call(t, tr, provider.ClassCredential)
	tr.Enable("d")
	if !tr.Healthy("d") {
		t.Fatal("Enable lifts a disable")
	}
}

func TestBulkheadAndSamples(t *testing.T) {
	tr, _ := newTracker()
	a, _ := tr.Acquire("d", 1)
	if _, err := tr.Acquire("d", 1); refusedFor(err) != "concurrency_full" {
		t.Fatal("a full bulkhead refuses at once")
	}
	a.Record("", 2*time.Second, true, false)
	a.Release()
	a.Release() // idempotent
	if tr.Stats("d").InFlight != 0 {
		t.Fatal("release frees the slot exactly once")
	}
	for i := range 5 {
		call(t, tr, "")
		_ = i
	}
	st := tr.Stats("d")
	if len(st.Latency) != 3 || len(st.TTFT) != 1 || st.OKThisMinute != 6 {
		t.Fatalf("bad stats: %+v", st)
	}
}
