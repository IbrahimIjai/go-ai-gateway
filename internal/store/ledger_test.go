package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func funded(t *testing.T, s *Store, micros int64) Project {
	t.Helper()
	p := project(t, s)
	if err := s.Grant(context.Background(), p.ID, micros, "grant-"+p.ID); err != nil {
		t.Fatal(err)
	}
	return p
}

// balanced is checked after every ledger test: the books always balance.
func balanced(t *testing.T, s *Store) {
	t.Helper()
	if bad, err := s.Unbalanced(context.Background()); err != nil || len(bad) > 0 {
		t.Fatalf("unbalanced transactions %v %v", bad, err)
	}
}

func balance(t *testing.T, s *Store, p Project) Balance {
	t.Helper()
	b, err := s.Balance(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGrantIsIdempotent(t *testing.T) {
	s := open(t)
	p := funded(t, s, 10_000_000)
	if err := s.Grant(context.Background(), p.ID, 10_000_000, "grant-"+p.ID); err != nil {
		t.Fatal(err)
	}
	if b := balance(t, s, p); b.BalanceMicros != 10_000_000 {
		t.Fatalf("the same grant twice credits once: %+v", b)
	}
	balanced(t, s)
}

// Two concurrent calls against $0.10 with $0.08 holds: exactly one proceeds.
func TestConcurrentHoldsCannotOverspend(t *testing.T) {
	s := open(t)
	p := funded(t, s, 100_000)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- s.Reserve(context.Background(), p.ID, p.ID+"-race-"+string(rune('a'+i)), 80_000, time.Minute)
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrInsufficientFunds):
			t.Fatal(err)
		}
	}
	if b := balance(t, s, p); ok != 1 || b.ReservedMicros != 80_000 || b.AvailableMicros != 20_000 {
		t.Fatalf("want exactly one hold, got %d: %+v", ok, b)
	}
}

func TestSettleChargesActualCostExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	p := funded(t, s, 1_000_000)
	id := p.ID + "-settle"
	if err := s.Reserve(ctx, p.ID, id, 300_000, time.Minute); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 5 { // duplicate settlement attempts
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Settle(ctx, id, 60_000) }()
	}
	wg.Wait()
	if b := balance(t, s, p); b.BalanceMicros != 940_000 || b.ReservedMicros != 0 {
		t.Fatalf("one charge of the actual cost, the rest released: %+v", b)
	}
	entries, _ := s.Ledger(ctx, p.ID, 10)
	if len(entries) != 2 || entries[0].Kind != "inference_settle" || entries[0].Micros != -60_000 {
		t.Fatalf("statement: %+v", entries)
	}
	balanced(t, s)
}

func TestReleaseAndUnknownCost(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	p := funded(t, s, 1_000_000)
	s.Reserve(ctx, p.ID, p.ID+"-fail", 400_000, time.Minute)
	if err := s.Release(ctx, p.ID+"-fail"); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, p.ID+"-fail", 1); err != nil || balance(t, s, p).BalanceMicros != 1_000_000 {
		t.Fatal("a released hold is never charged later")
	}
	s.Reserve(ctx, p.ID, p.ID+"-cut", 400_000, time.Minute)
	if err := s.Hold(ctx, p.ID+"-cut", "stream cut"); err != nil {
		t.Fatal(err)
	}
	if b := balance(t, s, p); b.ReservedMicros != 400_000 {
		t.Fatalf("an unknown cost keeps its hold until reviewed: %+v", b)
	}
	balanced(t, s)
}

func TestCostAboveHoldGoesToReview(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	p := funded(t, s, 1_000_000)
	s.Reserve(ctx, p.ID, p.ID+"-over", 100_000, time.Minute)
	if err := s.Settle(ctx, p.ID+"-over", 150_000); err != nil {
		t.Fatal(err)
	}
	var status, note string
	s.pool.QueryRow(ctx, `SELECT status, note FROM credit_reservations WHERE inference_request_id = $1`, p.ID+"-over").Scan(&status, &note)
	if b := balance(t, s, p); b.BalanceMicros != 900_000 || status != "pending_review" || note == "" {
		t.Fatalf("charge the hold, flag the rest: %+v %s %q", b, status, note)
	}
	balanced(t, s)
}

func TestIdempotencyKeys(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	p := project(t, s)
	if _, _, err := s.Claim(ctx, p.ID, "k1", []byte(`{"a":1}`), "req-1"); err != nil {
		t.Fatal(err)
	}
	original, same, err := s.Claim(ctx, p.ID, "k1", []byte(`{"a":1}`), "req-2")
	if !errors.Is(err, ErrDuplicate) || original != "req-1" || !same {
		t.Fatalf("a replay names the original: %s %v %v", original, same, err)
	}
	if _, same, err := s.Claim(ctx, p.ID, "k1", []byte(`{"a":2}`), "req-3"); !errors.Is(err, ErrDuplicate) || same {
		t.Fatal("a different body under the same key is a conflict")
	}
}

func TestWorkerResolvesExpiredHolds(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	p := funded(t, s, 1_000_000)
	cost := int64(50_000)
	for _, r := range []Record{
		{ID: p.ID + "-w-failed", Requested: "m", Status: "failed"},
		{ID: p.ID + "-w-ok", Requested: "m", Status: "ok", CostMicros: &cost},
	} {
		s.Reserve(ctx, p.ID, r.ID, 200_000, time.Second)
		if r.Status == "ok" {
			r.Usage = &coreUsage
		}
		if err := s.SaveRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	s.Reserve(ctx, p.ID, p.ID+"-w-crashed", 200_000, time.Second) // no record: the gateway died
	s.pool.Exec(ctx, `UPDATE credit_reservations SET expires_at = now() - interval '1 second' WHERE inference_request_id LIKE $1`, p.ID+"-w-%")
	n, err := s.ExpireHolds(ctx, 100)
	if err != nil || n < 3 {
		t.Fatalf("expired %d: %v", n, err)
	}
	b := balance(t, s, p)
	if b.BalanceMicros != 950_000 || b.ReservedMicros != 200_000 {
		t.Fatalf("failed→released, ok→settled, unknown→kept for review: %+v", b)
	}
	balanced(t, s)
}
