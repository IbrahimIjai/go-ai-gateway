package api

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"example.com/minirouter/internal/billing"
	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/provider"
	"example.com/minirouter/internal/router"
)

// scripted is a provider whose behavior each test sets: an error for every
// call, or a stream that yields chunks and then ends with end.
type scripted struct {
	err    error
	chunks []core.Chunk
	end    error
}

func (scripted) Name() string { return "scripted" }
func (p scripted) Complete(context.Context, string, core.ChatRequest) (core.Completion, error) {
	return core.Completion{}, p.err
}
func (p scripted) Stream(context.Context, string, core.ChatRequest) (provider.Stream, error) {
	if p.err != nil {
		return nil, p.err
	}
	return &scriptedStream{chunks: p.chunks, end: p.end}, nil
}

type scriptedStream struct {
	chunks []core.Chunk
	end    error
}

func (s *scriptedStream) Recv() (core.Chunk, error) {
	if len(s.chunks) == 0 {
		return core.Chunk{}, s.end
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, nil
}
func (*scriptedStream) Close() error { return nil }

func serverWith(p provider.Provider) *Server {
	return &Server{Key: "local-test-key", Store: billing.NewStore(), Router: &router.Router{
		Providers: map[string]provider.Provider{"scripted": p},
		Models:    []core.Model{{ID: "scripted/model", Provider: "scripted"}},
	}}
}

func TestCutStreamIsNotReportedAsFinished(t *testing.T) {
	s := serverWith(scripted{chunks: []core.Chunk{{Text: "partial "}}, end: io.ErrUnexpectedEOF})
	rec := testPost(t, s, `{"model":"scripted/model","messages":[{"role":"user","content":"hi"}],"stream":true}`, "Bearer local-test-key")
	body := rec.Body.String()
	if !strings.Contains(body, "partial ") || !strings.Contains(body, "upstream stream interrupted") {
		t.Fatalf("want the partial text, then an error frame: %s", body)
	}
	if strings.Contains(body, "[DONE]") || strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("a cut stream was reported as finished: %s", body)
	}
	if got := s.Store.Summary().Recent[0].Status; got != "usage_unavailable" {
		t.Fatalf("want usage_unavailable, got %q", got)
	}
}

func TestUpstreamFailuresMapToHonestErrors(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		status  int
		message string
	}{
		{"overloaded", &provider.APIError{Status: 503}, 502, "try again later"},
		{"unreachable", &url.Error{Op: "Post", URL: "http://x", Err: errors.New("connection refused")}, 502, "try again later"},
		{"quota", &provider.APIError{Status: 429, Code: "insufficient_quota"}, 502, "quota"},
		{"bad provider key", &provider.APIError{Status: 401}, 502, "credentials"},
		{"invalid request", &provider.APIError{Status: 400}, 502, "HTTP 400"},
		{"timeout", &url.Error{Op: "Post", URL: "http://x", Err: context.DeadlineExceeded}, 504, "timed out"},
	}
	for _, tc := range cases {
		for _, stream := range []string{"false", "true"} {
			s := serverWith(scripted{err: tc.err})
			rec := testPost(t, s, `{"model":"scripted/model","messages":[{"role":"user","content":"hi"}],"stream":`+stream+`}`, "Bearer local-test-key")
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.message) {
				t.Errorf("%s (stream=%s): want %d containing %q, got %d %s", tc.name, stream, tc.status, tc.message, rec.Code, rec.Body.String())
			}
		}
	}
}
