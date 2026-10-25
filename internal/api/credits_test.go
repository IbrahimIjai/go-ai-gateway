package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/minirouter/internal/catalog"
	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/provider"
	"example.com/minirouter/internal/router"
	"example.com/minirouter/internal/store"
)

// fakeLedger keeps holds in memory and records how each one ended.
type fakeLedger struct {
	mu        sync.Mutex
	available int64
	holds     map[string]int64
	outcome   map[string]string
	claimed   map[string]string
}

func newLedger(available int64) *fakeLedger {
	return &fakeLedger{available: available, holds: map[string]int64{}, outcome: map[string]string{}, claimed: map[string]string{}}
}

func (f *fakeLedger) Reserve(_ context.Context, _, id string, micros int64, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if micros > f.available {
		return store.ErrInsufficientFunds
	}
	f.available -= micros
	f.holds[id] = micros
	return nil
}
func (f *fakeLedger) end(id, how string, refund int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcome[id] = how
	f.available += refund
	return nil
}
func (f *fakeLedger) Settle(_ context.Context, id string, cost int64) error {
	return f.end(id, "settled", f.holds[id]-cost)
}
func (f *fakeLedger) Release(_ context.Context, id string) error {
	return f.end(id, "released", f.holds[id])
}
func (f *fakeLedger) Hold(_ context.Context, id, _ string) error { return f.end(id, "pending", 0) }
func (f *fakeLedger) Claim(_ context.Context, _, key string, _ []byte, id string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if prev, ok := f.claimed[key]; ok {
		return prev, true, store.ErrDuplicate
	}
	f.claimed[key] = id
	return "", false, nil
}

// pricedServer serves "m" at $1/$2 per million tokens, credit enforced.
func pricedServer(p provider.Provider, ledger *fakeLedger) *Server {
	snap, _ := catalog.NewSnapshot(catalog.Config{
		Models:      []catalog.Model{{ID: "m", Capabilities: catalog.Capabilities{Tools: true}}, {ID: "free"}},
		Deployments: []catalog.Deployment{{ID: "m", Model: "m", Provider: "p", UpstreamModel: "x", Price: &catalog.PriceCard{Input: "1", Output: "2", VerifiedAt: "2026-10-25T00:00:00Z"}}, {ID: "free", Model: "free", Provider: "p", UpstreamModel: "x"}},
	})
	s, _ := tenantServer()
	s.Router = &router.Router{Catalog: catalog.NewRegistry(snap), Providers: map[string]provider.Provider{"p": p}, Sleep: noSleep}
	s.Credits = ledger
	return s
}

const mReq = `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":1000}`

func TestCreditsSettleActualCost(t *testing.T) {
	ledger := newLedger(10_000_000)
	s := pricedServer(provider.Demo{}, ledger)
	s.Keys = fakeKeys{"key-c": {ID: "kc", ProjectID: "pc"}}
	rec := testPost(t, s, mReq, "Bearer key-c")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if len(ledger.outcome) != 1 {
		t.Fatalf("one hold, one outcome: %v", ledger.outcome)
	}
	for id, how := range ledger.outcome {
		// Hold: (16+2 input bytes)×$1 + 1000×$2 per million = 2018 µ$; the
		// demo used a few tokens, so most of it comes back.
		if how != "settled" || ledger.holds[id] != 2018 || ledger.available <= 10_000_000-2018 {
			t.Fatalf("settle: %s hold %d available %d", how, ledger.holds[id], ledger.available)
		}
	}
}

func TestCreditsRefuseAndRelease(t *testing.T) {
	ledger := newLedger(1000) // less than the 2018 µ$ hold
	s := pricedServer(provider.Demo{}, ledger)
	s.Keys = fakeKeys{"key-c": {ID: "kc", ProjectID: "pc"}}
	if rec := testPost(t, s, mReq, "Bearer key-c"); rec.Code != 402 || !strings.Contains(rec.Body.String(), "0.002018") {
		t.Fatalf("want 402 naming the maximum cost: %d %s", rec.Code, rec.Body.String())
	}
	if rec := testPost(t, s, strings.Replace(mReq, `"m"`, `"free"`, 1), "Bearer key-c"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "no verified price") {
		t.Fatalf("an unpriced model cannot run on credit: %d %s", rec.Code, rec.Body.String())
	}
	ledger.available = 10_000_000
	s.Router.Providers["p"] = &failing{err: &provider.APIError{Status: 500}}
	testPost(t, s, mReq, "Bearer key-c")
	if len(ledger.outcome) != 1 {
		t.Fatalf("refusals take no hold; the failed call's hold has one outcome: %v", ledger.outcome)
	}
	for _, how := range ledger.outcome {
		if how != "released" || ledger.available != 10_000_000 {
			t.Fatalf("a failed call releases its hold: %s %d", how, ledger.available)
		}
	}
}

func TestCutStreamKeepsHoldForReview(t *testing.T) {
	ledger := newLedger(10_000_000)
	s := pricedServer(scripted{chunks: []core.Chunk{{Text: "partial"}}, end: io.ErrUnexpectedEOF}, ledger)
	s.Keys = fakeKeys{"key-c": {ID: "kc", ProjectID: "pc"}}
	testPost(t, s, strings.Replace(mReq, `"max_completion_tokens"`, `"stream":true,"max_completion_tokens"`, 1), "Bearer key-c")
	for _, how := range ledger.outcome {
		if how != "pending" {
			t.Fatalf("unknown cost is not zero: %s", how)
		}
	}
	if len(ledger.outcome) != 1 {
		t.Fatalf("want one outcome: %v", ledger.outcome)
	}
}

func TestIdempotencyKeyConflict(t *testing.T) {
	s := pricedServer(provider.Demo{}, newLedger(10_000_000))
	s.Keys = fakeKeys{"key-c": {ID: "kc", ProjectID: "pc"}}
	post := func() int {
		req := httptestPost(mReq, "Bearer key-c")
		req.Header.Set("Idempotency-Key", "abc")
		return serve(s, req).Code
	}
	if first, second := post(), post(); first != 200 || second != 409 {
		t.Fatalf("want 200 then 409, got %d %d", first, second)
	}
}

func httptestPost(body, auth string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", auth)
	return r
}

func serve(s *Server, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

type failing struct{ err error }

func (failing) Name() string { return "failing" }
func (f *failing) Complete(context.Context, string, core.ChatRequest) (core.Completion, error) {
	return core.Completion{}, f.err
}
func (f *failing) Stream(context.Context, string, core.ChatRequest) (provider.Stream, error) {
	return nil, f.err
}
