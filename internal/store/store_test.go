package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/limits"
)

// open connects to MINIROUTER_TEST_DATABASE_URL, a disposable database;
// without it the store tests are skipped.
func open(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("MINIROUTER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set MINIROUTER_TEST_DATABASE_URL to a disposable PostgreSQL database")
	}
	s, err := Open(context.Background(), url, []byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func project(t *testing.T, s *Store) Project {
	t.Helper()
	p, err := s.CreateProject(context.Background(), "org-"+t.Name(), "p-"+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMigrateIsIdempotentAndSafeConcurrently(t *testing.T) {
	s := open(t)
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Migrate(context.Background()) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestKeysLifecycle(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	p := project(t, s)
	k, raw, err := s.CreateKey(ctx, Key{ProjectID: p.ID, Name: "ci", Models: []string{"router/auto"}, Limits: limits.Limits{RPM: 60}})
	if err != nil || !strings.HasPrefix(raw, k.Prefix+"_") || len(raw) < 40 {
		t.Fatalf("bad key %q %+v %v", raw, k, err)
	}
	var stored int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE position($1 in encode(key_digest, 'escape')) > 0 OR key_prefix = $1`, raw).Scan(&stored)
	if stored != 0 {
		t.Fatal("the raw key must not be stored")
	}
	got, err := s.Authenticate(ctx, raw)
	if err != nil || got.ID != k.ID || got.Limits.RPM != 60 || got.Models[0] != "router/auto" {
		t.Fatalf("authenticate: %+v %v", got, err)
	}
	if _, err := s.Authenticate(ctx, raw+"x"); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("a wrong key must fail")
	}
	if err := s.RevokeKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, raw); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("a revoked key must fail")
	}
	if err := s.RevokeKey(ctx, k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoking twice reports not found")
	}
	past := time.Now().Add(-time.Minute)
	_, old, _ := s.CreateKey(ctx, Key{ProjectID: p.ID, Name: "old", ExpiresAt: &past})
	if _, err := s.Authenticate(ctx, old); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("an expired key must fail")
	}
	keys, err := s.ListKeys(ctx, p.ID)
	if err != nil || len(keys) != 2 || keys[0].RevokedAt == nil {
		t.Fatalf("list: %+v %v", keys, err)
	}
}

func TestRequestRecordsAndUsage(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	p := project(t, s)
	cost := int64(1234)
	id := "chatcmpl-test-" + time.Now().Format("150405.000000")
	r := Record{ID: id, ProjectID: p.ID, Requested: "router/auto", Resolved: "m", Deployment: "m@a", Status: "ok",
		Route: core.RouteDecision{Requested: "router/auto", Policy: "alias:auto", Attempts: []core.Attempt{{Deployment: "m@b", Class: "rate_limit", Status: 429, Millis: 12}, {Deployment: "m@a", Millis: 80}}, Reasons: []string{"failover_rate_limit:m@b"}},
		Usage: &core.Usage{InputTokens: 100, OutputTokens: 20, CachedInput: 50}, CostMicros: &cost}
	if err := s.SaveRequest(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRequest(ctx, Record{ID: id + "-f", ProjectID: p.ID, Requested: "m", Status: "failed", ErrorClass: "transient", Route: core.RouteDecision{Attempts: []core.Attempt{}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRequest(ctx, Record{ID: id + "-u", ProjectID: p.ID, Requested: "m", Resolved: "m", Status: "ok", Route: core.RouteDecision{}, Usage: &core.Usage{InputTokens: 1, OutputTokens: 1}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Request(ctx, id)
	if err != nil || got.Usage.TotalTokens != 120 || *got.CostMicros != 1234 || len(got.Route.Attempts) != 2 || got.Route.Attempts[0].Status != 429 {
		t.Fatalf("request: %+v %v", got, err)
	}
	var attempts int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM provider_attempts WHERE inference_request_id = $1`, id).Scan(&attempts)
	if attempts != 2 {
		t.Fatalf("want 2 attempt rows, got %d", attempts)
	}
	rows, err := s.Usage(ctx, p.ID, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || len(rows) != 1 || rows[0].Requests != 3 || rows[0].Failed != 1 || rows[0].CostMicros != 1234 || rows[0].Unpriced != 1 || rows[0].InputTokens != 101 {
		t.Fatalf("usage: %+v %v", rows, err)
	}
	if _, err := s.Request(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing request reports not found")
	}
}

var coreUsage = core.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
