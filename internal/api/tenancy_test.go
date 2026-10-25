package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/minirouter/internal/limits"
	"example.com/minirouter/internal/store"
)

type fakeKeys map[string]store.Key

func (f fakeKeys) Authenticate(_ context.Context, raw string) (store.Key, error) {
	if k, ok := f[raw]; ok {
		return k, nil
	}
	return store.Key{}, store.ErrInvalidKey
}

type fakeRecords struct {
	mu    sync.Mutex
	saved []store.Record
}

func (f *fakeRecords) SaveRequest(_ context.Context, r store.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, r)
	return nil
}

func tenantServer() (*Server, *fakeRecords) {
	s := makeTestServer()
	recs := &fakeRecords{}
	s.Key = ""
	s.Keys = fakeKeys{
		"key-a": {ID: "ka", ProjectID: "pa", Limits: limits.Limits{RPM: 1}},
		"key-b": {ID: "kb", ProjectID: "pb", Models: []string{"router/auto"}},
	}
	s.Records, s.Limiter = recs, limits.New(nil)
	return s, recs
}

const hi = `{"model":"demo/echo","messages":[{"role":"user","content":"hi"}]}`

func TestPerKeyLimitsAndAllowlist(t *testing.T) {
	s, recs := tenantServer()
	if rec := testPost(t, s, hi, "Bearer key-a"); rec.Code != 200 {
		t.Fatalf("first call: %d", rec.Code)
	}
	rec := testPost(t, s, hi, "Bearer key-a")
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "rpm limit") {
		t.Fatalf("want 429 with Retry-After: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	if rec := testPost(t, s, hi, "Bearer key-b"); rec.Code != 403 {
		t.Fatalf("key-b may only use router/auto: %d", rec.Code)
	}
	if rec := testPost(t, s, strings.Replace(hi, "demo/echo", "router/auto", 1), "Bearer key-b"); rec.Code != 200 {
		t.Fatalf("allowed alias: %d %s", rec.Code, rec.Body.String())
	}
	if rec := testPost(t, s, hi, "Bearer unknown"); rec.Code != 401 {
		t.Fatalf("unknown key: %d", rec.Code)
	}
	if len(recs.saved) != 2 || recs.saved[0].ProjectID != "pa" || recs.saved[1].KeyID != "kb" || recs.saved[1].Route.Deployment != "demo/echo" || recs.saved[0].CostMicros != nil {
		t.Fatalf("records carry project, key and route; an unpriced call has no cost (unknown, not zero): %+v", recs.saved)
	}
}

func TestFailedRequestsAreRecorded(t *testing.T) {
	s, recs := tenantServer()
	testPost(t, s, `{"model":"demo/echo","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`, "Bearer key-a")
	if len(recs.saved) != 1 || recs.saved[0].Status != "failed" || recs.saved[0].Usage != nil {
		t.Fatalf("a refused request is recorded as failed, without usage: %+v", recs.saved)
	}
}

type fakeAdmin struct{ revoked string }

func (f *fakeAdmin) CreateProject(_ context.Context, org, name string) (store.Project, error) {
	return store.Project{ID: "p1", Organization: org, Name: name}, nil
}
func (f *fakeAdmin) CreateKey(_ context.Context, k store.Key) (store.Key, string, error) {
	k.ID, k.Prefix = "k1", "mr_abcd1234"
	return k, "mr_abcd1234_secret", nil
}
func (f *fakeAdmin) ListKeys(context.Context, string) ([]store.Key, error) { return nil, nil }
func (f *fakeAdmin) RevokeKey(_ context.Context, id string) error {
	if id != "k1" {
		return store.ErrNotFound
	}
	f.revoked = id
	return nil
}
func (f *fakeAdmin) Usage(context.Context, string, time.Time, time.Time) ([]store.UsageRow, error) {
	return nil, nil
}
func (f *fakeAdmin) Request(context.Context, string) (store.Record, error) {
	return store.Record{}, store.ErrNotFound
}

func TestAdminAPI(t *testing.T) {
	s, _ := tenantServer()
	fa := &fakeAdmin{}
	s.Admin = &Admin{Key: "admin-key-admin-key-12345", Store: fa}
	call := func(method, path, body, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	admin := "Bearer admin-key-admin-key-12345"
	if rec := call("POST", "/admin/v1/keys", `{"project_id":"p1","name":"ci"}`, "Bearer key-a"); rec.Code != 401 {
		t.Fatalf("an inference key never opens the admin API: %d", rec.Code)
	}
	rec := call("POST", "/admin/v1/keys", `{"project_id":"p1","name":"ci","rpm":5}`, admin)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"secret":"mr_abcd1234_secret"`) {
		t.Fatalf("create key: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call("POST", "/admin/v1/keys", `{"project_id":"p1","name":"ci","rpmm":5}`, admin); rec.Code != 400 {
		t.Fatal("unknown fields are rejected")
	}
	if rec := call("DELETE", "/admin/v1/keys/k1", "", admin); rec.Code != 204 || fa.revoked != "k1" {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := call("GET", "/admin/v1/requests/nope", "", admin); rec.Code != 404 {
		t.Fatalf("missing request: %d", rec.Code)
	}
	if rec := call("GET", "/admin/v1/deployments", "", admin); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"demo/echo":{"breaker":"closed"`) {
		t.Fatalf("deployments: %d %s", rec.Code, rec.Body.String())
	}
}
