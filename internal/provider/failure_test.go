package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestClassifyUpstreamErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   FailureClass
	}{
		{"openai insufficient quota", 429, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`, ClassQuota},
		{"openai rate limit", 429, `{"error":{"message":"Rate limit reached","type":"requests","param":null,"code":"rate_limit_exceeded"}}`, ClassRateLimit},
		{"openai bad key", 401, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`, ClassCredential},
		{"openai unknown model", 404, `{"error":{"message":"The model does not exist","type":"invalid_request_error","param":null,"code":"model_not_found"}}`, ClassModelGone},
		{"anthropic overloaded", 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, ClassTransient},
		{"anthropic credit balance", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`, ClassQuota},
		{"anthropic invalid request", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: too large"}}`, ClassCallerFault},
		{"gemini resource exhausted", 429, `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`, ClassRateLimit},
		{"gemini billing disabled", 400, `{"error":{"code":400,"message":"This API method requires billing to be enabled.","status":"FAILED_PRECONDITION"}}`, ClassQuota},
		{"html from a proxy", 502, `<html><body>Bad Gateway</body></html>`, ClassTransient},
		{"request timeout", 408, ``, ClassTimeout},
		{"openai context length", 400, `{"error":{"message":"This model's maximum context length is 128000 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`, ClassContext},
		{"anthropic prompt too long", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`, ClassContext},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			_, err := (&OpenAI{Key: "k", BaseURL: srv.URL, Client: srv.Client()}).Complete(context.Background(), "m", testReq())
			if got := Class(err); got != tc.want {
				t.Fatalf("want %s, got %s (%v)", tc.want, got, err)
			}
		})
	}
}

func TestUpstreamErrorTextIsNeverEchoed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"echoed key sk-live-123 and prompt text"}}`)
	}))
	defer srv.Close()
	_, err := (&OpenAI{Key: "k", BaseURL: srv.URL, Client: srv.Client()}).Complete(context.Background(), "m", testReq())
	var api *APIError
	if !errors.As(err, &api) || api.Code != "invalid_request_error" {
		t.Fatalf("want an APIError with the provider code, got %v", err)
	}
	if strings.Contains(err.Error(), "sk-live-123") || strings.Contains(err.Error(), "prompt") {
		t.Fatalf("upstream body leaked into the error: %v", err)
	}
}

func TestClassifyTransportErrors(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		want      FailureClass
		retryable bool
	}{
		{"connection refused", &url.Error{Op: "Post", URL: "http://x", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, ClassTransient, true},
		{"deadline", &url.Error{Op: "Post", URL: "http://x", Err: context.DeadlineExceeded}, ClassTimeout, true},
		{"client went away", &url.Error{Op: "Post", URL: "http://x", Err: context.Canceled}, ClassCancelled, false},
		{"cut stream", io.ErrUnexpectedEOF, ClassTransient, true},
		{"undecodable response", errors.New("invalid character '<'"), ClassUnknown, false},
		{"bare 429", &APIError{Status: 429}, ClassRateLimit, true},
		{"bare 400", &APIError{Status: 400}, ClassCallerFault, false},
	}
	for _, tc := range cases {
		if got := Class(tc.err); got != tc.want || Retryable(tc.err) != tc.retryable {
			t.Errorf("%s: want %s/retryable=%v, got %s/retryable=%v", tc.name, tc.want, tc.retryable, got, Retryable(tc.err))
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{"milliseconds", http.Header{"Retry-After-Ms": {"1500"}}, 1500 * time.Millisecond},
		{"seconds", http.Header{"Retry-After": {"7"}}, 7 * time.Second},
		{"http date", http.Header{"Retry-After": {now.Add(30 * time.Second).Format(http.TimeFormat)}}, 30 * time.Second},
		{"absurd value is capped", http.Header{"Retry-After": {"99999999999999"}}, maxRetryAfter},
		{"garbage", http.Header{"Retry-After": {"soon"}}, 0},
		{"absent", http.Header{}, 0},
	}
	for _, tc := range cases {
		if got := retryAfter(tc.header, now); got != tc.want {
			t.Errorf("%s: want %s, got %s", tc.name, tc.want, got)
		}
	}
}
