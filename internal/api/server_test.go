package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/minirouter/internal/billing"
	"example.com/minirouter/internal/catalog"
	"example.com/minirouter/internal/provider"
	"example.com/minirouter/internal/router"
)

func makeTestServer() *Server {
	return &Server{Key: "local-test-key", Store: billing.NewStore(), Router: &router.Router{Providers: map[string]provider.Provider{"demo/echo": provider.Demo{}}, Catalog: catalog.Static("demo/echo"), Sleep: noSleep}}
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
func TestBearerSchemeIsRequired(t *testing.T) {
	s := makeTestServer()
	body := `{"model":"demo/echo","messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		header string
		want   int
	}{
		{"Bearer local-test-key", 200},
		{"bearer local-test-key", 200}, // the scheme is case-insensitive (RFC 7235)
		{"local-test-key", 401},
		{"Basic local-test-key", 401},
		{"Bearer ", 401},
		{"Bearer local-test-key-extra", 401},
	}
	for _, tc := range cases {
		if rec := testPost(t, s, body, tc.header); rec.Code != tc.want {
			t.Errorf("%q: want %d, got %d", tc.header, tc.want, rec.Code)
		}
	}
}
func TestModelsListsCatalog(t *testing.T) {
	s := makeTestServer()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer local-test-key")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{`"id":"demo/echo"`, `"capabilities":{"tools":true,"structured_output":false}`, `"deployments":[{"id":"demo/echo"`, `"id":"router/auto"`, `"policy":"ordered"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
}
