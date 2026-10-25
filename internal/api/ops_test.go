package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/provider"
)

// slowStream sends one word, stays silent, then finishes.
type slowStream struct{ step int }

func (s *slowStream) Recv() (core.Chunk, error) {
	s.step++
	switch s.step {
	case 1:
		return core.Chunk{Text: "thinking"}, nil
	case 2:
		time.Sleep(120 * time.Millisecond) // quiet long enough for keep-alives
		return core.Chunk{Text: " done", FinishReason: "stop"}, nil
	}
	return core.Chunk{}, io.EOF
}
func (*slowStream) Close() error { return nil }

type slowProvider struct{}

func (slowProvider) Name() string { return "slow" }
func (slowProvider) Complete(context.Context, string, core.ChatRequest) (core.Completion, error) {
	return core.Completion{}, nil
}
func (slowProvider) Stream(context.Context, string, core.ChatRequest) (provider.Stream, error) {
	return &slowStream{}, nil
}

func TestKeepAliveCoversSilence(t *testing.T) {
	s := serverWith(slowProvider{})
	s.KeepAlive = 30 * time.Millisecond
	rec := testPost(t, s, `{"model":"scripted/model","messages":[{"role":"user","content":"hi"}],"stream":true}`, "Bearer local-test-key")
	body := rec.Body.String()
	first, last := strings.Index(body, "thinking"), strings.Index(body, " done")
	keep := strings.Index(body, ": keep-alive\n\n")
	if keep < first || keep > last || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("want keep-alive comments between the words and a normal end:\n%s", body)
	}
}

func TestReadiness(t *testing.T) {
	s := makeTestServer()
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec
	}
	if rec := get(); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"deployments_available":1`) {
		t.Fatalf("ready: %d %s", rec.Code, rec.Body.String())
	}
	s.Ping = func(context.Context) error { return errors.New("connection refused to 10.0.0.5") }
	rec := get()
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), `"database":"unavailable"`) || strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("a dead database means not ready, without leaking details: %d %s", rec.Code, rec.Body.String())
	}
	s.Ping = nil
	for range 5 { // trip the only deployment's breaker
		k, _ := s.Router.Tracker().Acquire("demo/echo", 0)
		k.Record(provider.ClassTransient, 0, false, false)
		k.Release()
	}
	if rec := get(); rec.Code != 503 || !strings.Contains(rec.Body.String(), `"deployments_available":0`) {
		t.Fatalf("no available deployment means not ready: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRequestIDs(t *testing.T) {
	s := makeTestServer()
	call := func(id string) string {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		if id != "" {
			req.Header.Set("X-Request-ID", id)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Header().Get("X-Request-ID")
	}
	if got := call("trace-123"); got != "trace-123" {
		t.Fatalf("a plain caller id is kept: %q", got)
	}
	if got := call("bad id\nwith newline"); got == "" || strings.ContainsAny(got, " \n") {
		t.Fatalf("an unsafe id is replaced: %q", got)
	}
	if a, b := call(""), call(""); a == "" || a == b {
		t.Fatal("generated ids are unique")
	}
}
