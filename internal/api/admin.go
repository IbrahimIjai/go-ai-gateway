package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"example.com/minirouter/internal/billing"
	"example.com/minirouter/internal/health"
	"example.com/minirouter/internal/limits"
	"example.com/minirouter/internal/store"
)

// AdminStore is what the admin API needs from PostgreSQL.
type AdminStore interface {
	CreateProject(ctx context.Context, org, name string) (store.Project, error)
	CreateKey(ctx context.Context, k store.Key) (store.Key, string, error)
	ListKeys(ctx context.Context, projectID string) ([]store.Key, error)
	RevokeKey(ctx context.Context, id string) error
	Usage(ctx context.Context, projectID string, from, to time.Time) ([]store.UsageRow, error)
	Request(ctx context.Context, id string) (store.Record, error)
	Grant(ctx context.Context, projectID string, micros int64, reference string) error
	Balance(ctx context.Context, projectID string) (store.Balance, error)
	Ledger(ctx context.Context, projectID string, limit int) ([]store.LedgerEntry, error)
}

// Admin serves /admin/v1 with its own credential: an inference key never
// grants admin access.
type Admin struct {
	Key   string
	Store AdminStore
	Log   interface{ Error(string, ...any) }
}

func (a *Admin) mount(mux *http.ServeMux, s *Server) {
	mux.HandleFunc("POST /admin/v1/projects", a.auth(a.createProject))
	mux.HandleFunc("POST /admin/v1/keys", a.auth(a.createKey))
	mux.HandleFunc("GET /admin/v1/keys", a.auth(a.listKeys))
	mux.HandleFunc("DELETE /admin/v1/keys/{id}", a.auth(a.revokeKey))
	mux.HandleFunc("GET /admin/v1/usage", a.auth(a.usage))
	mux.HandleFunc("GET /admin/v1/requests/{id}", a.auth(a.request))
	mux.HandleFunc("POST /admin/v1/credits", a.auth(a.grant))
	mux.HandleFunc("GET /admin/v1/balance", a.auth(func(w http.ResponseWriter, r *http.Request) { a.balance(w, r) }))
	mux.HandleFunc("GET /admin/v1/ledger", a.auth(a.ledger))
	mux.HandleFunc("GET /admin/v1/deployments", a.auth(func(w http.ResponseWriter, r *http.Request) { a.deployments(w, s) }))
	mux.HandleFunc("POST /admin/v1/deployments/{id}/enable", a.auth(func(w http.ResponseWriter, r *http.Request) {
		s.Router.Tracker().Enable(r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (a *Admin) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r)
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(a.Key)) != 1 {
			jsonError(w, http.StatusUnauthorized, "invalid_admin_key", "Missing or invalid admin key")
			return
		}
		next(w, r)
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON or unsupported field: "+err.Error())
		return false
	}
	return true
}

func (a *Admin) failed(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		jsonError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if a.Log != nil {
		a.Log.Error("admin request failed", "error", err)
	}
	jsonError(w, http.StatusInternalServerError, "internal_error", "storage error")
}

func (a *Admin) createProject(w http.ResponseWriter, r *http.Request) {
	var in struct{ Organization, Name string }
	if !readJSON(w, r, &in) {
		return
	}
	if in.Organization == "" || in.Name == "" {
		jsonError(w, http.StatusBadRequest, "invalid_request_error", "organization and name are required")
		return
	}
	p, err := a.Store.CreateProject(r.Context(), in.Organization, in.Name)
	if err != nil {
		a.failed(w, err)
		return
	}
	jsonOut(w, http.StatusCreated, p)
}

func (a *Admin) createKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProjectID   string     `json:"project_id"`
		Name        string     `json:"name"`
		Models      []string   `json:"models"`
		ExpiresAt   *time.Time `json:"expires_at"`
		RPM         int        `json:"rpm"`
		TPM         int        `json:"tpm"`
		Concurrency int        `json:"concurrency"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.ProjectID == "" || in.Name == "" || in.RPM < 0 || in.TPM < 0 || in.Concurrency < 0 {
		jsonError(w, http.StatusBadRequest, "invalid_request_error", "project_id and name are required; limits must be ≥ 0")
		return
	}
	k, raw, err := a.Store.CreateKey(r.Context(), store.Key{ProjectID: in.ProjectID, Name: in.Name, Models: in.Models, ExpiresAt: in.ExpiresAt,
		Limits: limits.Limits{RPM: in.RPM, TPM: in.TPM, Concurrency: in.Concurrency}})
	if err != nil {
		a.failed(w, err)
		return
	}
	// The secret is shown once; only its digest is stored.
	jsonOut(w, http.StatusCreated, map[string]any{"key": k, "secret": raw})
}

func (a *Admin) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := a.Store.ListKeys(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		a.failed(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"data": keys})
}

func (a *Admin) revokeKey(w http.ResponseWriter, r *http.Request) {
	if err := a.Store.RevokeKey(r.Context(), r.PathValue("id")); err != nil {
		a.failed(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Admin) usage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	to, from := time.Now(), time.Now().AddDate(0, 0, -30)
	for _, f := range []struct {
		name string
		dst  *time.Time
	}{{"from", &from}, {"to", &to}} {
		if v := q.Get(f.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				jsonError(w, http.StatusBadRequest, "invalid_request_error", f.name+" must be RFC 3339")
				return
			}
			*f.dst = t
		}
	}
	rows, err := a.Store.Usage(r.Context(), q.Get("project_id"), from, to)
	if err != nil {
		a.failed(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"from": from, "to": to, "data": rows})
}

func (a *Admin) request(w http.ResponseWriter, r *http.Request) {
	rec, err := a.Store.Request(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failed(w, err)
		return
	}
	jsonOut(w, http.StatusOK, rec)
}

func (a *Admin) deployments(w http.ResponseWriter, s *Server) {
	out := map[string]health.Stats{}
	for _, d := range s.Router.Catalog.Load().Config.Deployments {
		out[d.ID] = s.Router.Tracker().Stats(d.ID)
	}
	jsonOut(w, http.StatusOK, map[string]any{"data": out})
}

// grant adds test or manual credit. Real top-ups need a verified payment
// webhook, which is not built; the reference keeps a retried grant single.
func (a *Admin) grant(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProjectID string `json:"project_id"`
		AmountUSD string `json:"amount_usd"`
		Reference string `json:"reference"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	micros, err := billing.ParseUSD(in.AmountUSD)
	if err != nil || micros <= 0 || in.ProjectID == "" || in.Reference == "" {
		jsonError(w, http.StatusBadRequest, "invalid_request_error", "project_id, a positive amount_usd such as \"10.00\", and a unique reference are required")
		return
	}
	if err := a.Store.Grant(r.Context(), in.ProjectID, micros, in.Reference); err != nil {
		a.failed(w, err)
		return
	}
	a.balance(w, r.WithContext(r.Context()), in.ProjectID)
}

func (a *Admin) balance(w http.ResponseWriter, r *http.Request, project ...string) {
	id := r.URL.Query().Get("project_id")
	if len(project) > 0 {
		id = project[0]
	}
	b, err := a.Store.Balance(r.Context(), id)
	if err != nil {
		a.failed(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"project_id": id, "currency": "USD", "balance": b,
		"available_usd": billing.FormatUSD(b.AvailableMicros)})
}

func (a *Admin) ledger(w http.ResponseWriter, r *http.Request) {
	entries, err := a.Store.Ledger(r.Context(), r.URL.Query().Get("project_id"), 200)
	if err != nil {
		a.failed(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"data": entries})
}
