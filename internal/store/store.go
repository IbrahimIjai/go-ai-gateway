// Package store persists tenancy, API keys and request records in
// PostgreSQL. Raw keys, prompts and outputs are never stored.
package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"example.com/minirouter/db/migrations"
	"example.com/minirouter/internal/core"
	"example.com/minirouter/internal/limits"
)

var (
	ErrInvalidKey = errors.New("invalid, revoked or expired API key")
	ErrNotFound   = errors.New("not found")
)

type Store struct {
	pool   *pgxpool.Pool
	pepper []byte // server secret mixed into key digests
}

// Open connects; pepper keys the HMAC that stands in for stored keys, so a
// leaked database alone cannot be used to test key guesses offline.
func Open(ctx context.Context, url string, pepper []byte) (*Store, error) {
	if len(pepper) < 32 {
		return nil, errors.New("key pepper must be at least 32 bytes")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool, pepper: pepper}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Migrate applies embedded migrations in order, each in its own transaction,
// under an advisory lock so concurrent instances cannot race.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	const lock = 7_310_217 // arbitrary, fixed advisory lock id for migrations
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lock); err != nil {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lock)
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations.Files, "*.up.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

type Project struct {
	ID           string `json:"id"`
	Organization string `json:"organization"`
	Name         string `json:"name"`
}

// CreateProject creates a project, and its organization if new.
func (s *Store) CreateProject(ctx context.Context, org, name string) (Project, error) {
	p := Project{Organization: org, Name: name}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var orgID string
		err := tx.QueryRow(ctx, `INSERT INTO organizations (name) VALUES ($1)
			ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name RETURNING id`, org).Scan(&orgID)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO projects (organization_id, name) VALUES ($1, $2) RETURNING id`, orgID, name).Scan(&p.ID)
	})
	return p, err
}

type Key struct {
	ID        string        `json:"id"`
	ProjectID string        `json:"project_id"`
	Name      string        `json:"name"`
	Prefix    string        `json:"prefix"`
	Models    []string      `json:"models"` // allowed model or alias IDs; empty: any
	Limits    limits.Limits `json:"limits"`
	ExpiresAt *time.Time    `json:"expires_at,omitempty"`
	RevokedAt *time.Time    `json:"revoked_at,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
}

var keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func (s *Store) digest(raw string) []byte {
	m := hmac.New(sha256.New, s.pepper)
	m.Write([]byte(raw))
	return m.Sum(nil)
}

// CreateKey issues a key and returns it raw exactly once: mr_<prefix>_<secret>
// with 160 random bits of secret. Only the prefix and an HMAC digest are kept.
func (s *Store) CreateKey(ctx context.Context, k Key) (Key, string, error) {
	var b [25]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Key{}, "", err
	}
	enc := strings.ToLower(keyEncoding.EncodeToString(b[:]))
	k.Prefix = "mr_" + enc[:8]
	raw := k.Prefix + "_" + enc[8:]
	if k.Models == nil {
		k.Models = []string{}
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO api_keys (project_id, name, key_prefix, key_digest, models, rpm, tpm, concurrency, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id, created_at`,
		k.ProjectID, k.Name, k.Prefix, s.digest(raw), k.Models, k.Limits.RPM, k.Limits.TPM, k.Limits.Concurrency, k.ExpiresAt).Scan(&k.ID, &k.CreatedAt)
	if err != nil {
		return Key{}, "", err
	}
	return k, raw, nil
}

const keyColumns = `id, project_id, name, key_prefix, models, rpm, tpm, concurrency, expires_at, revoked_at, created_at`

func scanKey(row pgx.Row) (Key, error) {
	var k Key
	err := row.Scan(&k.ID, &k.ProjectID, &k.Name, &k.Prefix, &k.Models, &k.Limits.RPM, &k.Limits.TPM, &k.Limits.Concurrency, &k.ExpiresAt, &k.RevokedAt, &k.CreatedAt)
	return k, err
}

// Authenticate resolves a raw key. Unknown, revoked and expired keys all fail
// the same way, so callers learn nothing about which it was.
func (s *Store) Authenticate(ctx context.Context, raw string) (Key, error) {
	k, err := scanKey(s.pool.QueryRow(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE key_digest = $1`, s.digest(raw)))
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (k.RevokedAt != nil || k.ExpiresAt != nil && !k.ExpiresAt.After(time.Now())) {
		return Key{}, ErrInvalidKey
	}
	return k, err
}

func (s *Store) ListKeys(ctx context.Context, projectID string) ([]Key, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE project_id = $1 ORDER BY created_at`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Key, error) { return scanKey(r) })
}

func (s *Store) RevokeKey(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// Record is one request's durable trace: who, what, where, how it went.
type Record struct {
	ID         string             `json:"id"`
	ProjectID  string             `json:"project_id,omitempty"`
	KeyID      string             `json:"api_key_id,omitempty"`
	Requested  string             `json:"requested_model"`
	Resolved   string             `json:"resolved_model,omitempty"`
	Deployment string             `json:"deployment_id,omitempty"`
	Status     string             `json:"status"` // ok | usage_unavailable | failed
	ErrorClass string             `json:"error_class,omitempty"`
	Stream     bool               `json:"stream"`
	Route      core.RouteDecision `json:"route"`
	Usage      *core.Usage        `json:"usage,omitempty"`
	CostMicros *int64             `json:"cost_micros,omitempty"` // nil: unpriced or unknown
	CreatedAt  time.Time          `json:"created_at"`
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SaveRequest writes a request, its attempts and its usage in one transaction.
func (s *Store) SaveRequest(ctx context.Context, r Record) error {
	route, err := json.Marshal(r.Route)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO inference_requests (id, project_id, api_key_id, requested_model, resolved_model, deployment_id, status, error_class, stream, route)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			r.ID, nullable(r.ProjectID), nullable(r.KeyID), r.Requested, nullable(r.Resolved), nullable(r.Deployment), r.Status, nullable(r.ErrorClass), r.Stream, route)
		if err != nil {
			return err
		}
		for i, a := range r.Route.Attempts {
			var status any
			if a.Status != 0 {
				status = a.Status
			}
			if _, err := tx.Exec(ctx, `INSERT INTO provider_attempts (inference_request_id, attempt_number, deployment_id, class, http_status, duration_ms)
				VALUES ($1, $2, $3, $4, $5, $6)`, r.ID, i+1, a.Deployment, nullable(a.Class), status, a.Millis); err != nil {
				return err
			}
		}
		if u := r.Usage; u != nil {
			raw := u.Raw
			if len(raw) == 0 {
				raw = json.RawMessage(`{}`)
			}
			_, err = tx.Exec(ctx, `INSERT INTO request_usage (inference_request_id, input_tokens, output_tokens, cached_read_tokens, cached_write_tokens, reasoning_tokens, cost_micros, provider_usage)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, r.ID, u.InputTokens, u.OutputTokens, u.CachedInput, u.CacheWrite, u.ReasoningTokens, r.CostMicros, raw)
		}
		return err
	})
}

// Request loads one request record with its usage.
func (s *Store) Request(ctx context.Context, id string) (Record, error) {
	var r Record
	var project, key, resolved, deployment, class *string
	var route []byte
	var in, out, read, write, reasoning *int64
	err := s.pool.QueryRow(ctx, `SELECT q.id, q.project_id::text, q.api_key_id::text, q.requested_model, q.resolved_model, q.deployment_id, q.status, q.error_class, q.stream, q.route, q.created_at,
			u.input_tokens, u.output_tokens, u.cached_read_tokens, u.cached_write_tokens, u.reasoning_tokens, u.cost_micros
		FROM inference_requests q LEFT JOIN request_usage u ON u.inference_request_id = q.id WHERE q.id = $1`, id).Scan(
		&r.ID, &project, &key, &r.Requested, &resolved, &deployment, &r.Status, &class, &r.Stream, &route, &r.CreatedAt,
		&in, &out, &read, &write, &reasoning, &r.CostMicros)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	r.ProjectID, r.KeyID, r.Resolved, r.Deployment, r.ErrorClass = deref(project), deref(key), deref(resolved), deref(deployment), deref(class)
	if in != nil {
		r.Usage = &core.Usage{InputTokens: *in, OutputTokens: *out, CachedInput: *read, CacheWrite: *write, ReasoningTokens: *reasoning}
		r.Usage.Normalize()
	}
	return r, json.Unmarshal(route, &r.Route)
}

// UsageRow aggregates one model's requests in a period. Unpriced requests are
// counted apart: their cost is unknown, not zero.
type UsageRow struct {
	Model        string `json:"model"`
	Requests     int64  `json:"requests"`
	Failed       int64  `json:"failed"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CostMicros   int64  `json:"known_cost_micros"`
	Unpriced     int64  `json:"unpriced_requests"`
}

func (s *Store) Usage(ctx context.Context, projectID string, from, to time.Time) ([]UsageRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT COALESCE(q.resolved_model, q.requested_model), count(*),
			count(*) FILTER (WHERE q.status = 'failed'),
			COALESCE(sum(u.input_tokens), 0), COALESCE(sum(u.output_tokens), 0),
			COALESCE(sum(u.cost_micros), 0),
			count(*) FILTER (WHERE q.status <> 'failed' AND u.cost_micros IS NULL)
		FROM inference_requests q LEFT JOIN request_usage u ON u.inference_request_id = q.id
		WHERE q.project_id = $1 AND q.created_at >= $2 AND q.created_at < $3
		GROUP BY 1 ORDER BY 1`, projectID, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (UsageRow, error) {
		var u UsageRow
		err := r.Scan(&u.Model, &u.Requests, &u.Failed, &u.InputTokens, &u.OutputTokens, &u.CostMicros, &u.Unpriced)
		return u, err
	})
}

// Ping reports whether the database answers, for readiness checks.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
