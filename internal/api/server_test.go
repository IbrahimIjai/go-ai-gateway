package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/minirouter/internal/billing"
	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/provider"
	"example.com/minirouter/internal/router"
)

func makeTestServer() *Server {
	return &Server{Key: "local-test-key", Store: billing.NewStore(), Router: &router.Router{Providers: map[string]provider.Provider{"demo": provider.Demo{}}, Models: []core.Model{{ID: "demo/echo", Provider: "demo", Price: &core.Price{}}}}}
}
func testPost(t *testing.T, s *Server, json string, auth string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(json))
	request.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, request)
	return rec
}
func TestAPIEndToEnd(t *testing.T) {
	s := makeTestServer()
	rec := testPost(t, s, `{"model":"demo/echo","messages":[{"role":"user","content":"hi"}]}`, "Bearer local-test-key")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "DEMO ONLY") {
		t.Fatalf("bad result: %d %s", rec.Code, rec.Body.String())
	}
	rec = testPost(t, s, `{"model":"demo/echo","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`, "Bearer local-test-key")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "[DONE]") || !strings.Contains(rec.Body.String(), `"choices":[]`) {
		t.Fatalf("bad SSE: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer local-test-key")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"requests":2`) {
		t.Fatalf("missing metering: %d %s", rec.Code, rec.Body.String())
	}
}
func TestAPIRejectsUnknownFeaturesAndBadAuth(t *testing.T) {
	s := makeTestServer()
	rec := testPost(t, s, `{"model":"demo/echo","messages":[{"role":"user","content":"hi"}]}`, "")
	if rec.Code != 401 {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	rec = testPost(t, s, `{"model":"demo/echo","messages":[{"role":"user","content":"hi"}],"tools":[{}]}`, "Bearer local-test-key")
	if rec.Code != 400 {
		t.Fatalf("tools must not be silently ignored: %d", rec.Code)
	}
	rec = testPost(t, s, `{"model":"oops","messages":[{"role":"user","content":"hi"}]}`, "Bearer local-test-key")
	if rec.Code != 404 {
		t.Fatalf("missing model should be 404: %d", rec.Code)
	}
}
