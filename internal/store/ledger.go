package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrInsufficientFunds = errors.New("insufficient credit for this request's maximum cost")
	ErrDuplicate         = errors.New("idempotency key already used")
)

// Balance is a project's credit in micro-USD. Available is what new holds
// may take: balance minus active holds.
type Balance struct {
	BalanceMicros   int64 `json:"balance_micros"`
	ReservedMicros  int64 `json:"reserved_micros"`
	AvailableMicros int64 `json:"available_micros"`
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// account returns the id of an account, creating it on first use.
func account(ctx context.Context, q querier, projectID, kind string) (string, error) {
	var id string
	var project any
	if projectID != "" {
		project = projectID
	}
	err := q.QueryRow(ctx, `WITH found AS (
			SELECT id FROM journal_accounts WHERE kind = $2 AND project_id IS NOT DISTINCT FROM $1::uuid
		), made AS (
			INSERT INTO journal_accounts (project_id, kind) SELECT $1::uuid, $2 WHERE NOT EXISTS (SELECT 1 FROM found)
			ON CONFLICT DO NOTHING RETURNING id
		)
		SELECT id FROM found UNION ALL SELECT id FROM made`, project, kind).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) { // lost a creation race; it exists now
		err = q.QueryRow(ctx, `SELECT id FROM journal_accounts WHERE kind = $2 AND project_id IS NOT DISTINCT FROM $1::uuid`, project, kind).Scan(&id)
	}
	return id, err
}

// post writes one balanced transaction: entries maps account id to amount.
func post(ctx context.Context, tx pgx.Tx, kind, requestID, reference, memo string, entries map[string]int64) error {
	sum := int64(0)
	for _, v := range entries {
		sum += v
	}
	if sum != 0 {
		return fmt.Errorf("unbalanced %s transaction: entries sum to %d", kind, sum)
	}
	var txID string
	if err := tx.QueryRow(ctx, `INSERT INTO journal_transactions (inference_request_id, kind, external_reference, memo)
		VALUES ($1, $2, $3, $4) RETURNING id`, nullable(requestID), kind, nullable(reference), nullable(memo)).Scan(&txID); err != nil {
		return err
	}
	for acct, amount := range entries {
		if amount == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (journal_transaction_id, journal_account_id, amount_micros) VALUES ($1, $2, $3)`, txID, acct, amount); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE journal_accounts SET balance_micros = balance_micros + $2 WHERE id = $1`, acct, amount); err != nil {
			return err
		}
	}
	return nil
}

// Grant adds credit to a project. The reference makes it idempotent: the
// same grant posted twice credits once.
func (s *Store) Grant(ctx context.Context, projectID string, micros int64, reference string) error {
	if micros <= 0 || reference == "" {
		return errors.New("a grant needs a positive amount and a reference")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM journal_transactions WHERE kind = 'credit_grant' AND external_reference = $1)`, reference).Scan(&exists); err != nil || exists {
			return err
		}
		customer, err := account(ctx, tx, projectID, "customer_credit")
		if err != nil {
			return err
		}
		grants, err := account(ctx, tx, "", "platform_grants")
		if err != nil {
			return err
		}
		return post(ctx, tx, "credit_grant", "", reference, "admin grant", map[string]int64{grants: -micros, customer: micros})
	})
}

func (s *Store) Balance(ctx context.Context, projectID string) (Balance, error) {
	var b Balance
	err := s.pool.QueryRow(ctx, `SELECT balance_micros, reserved_micros FROM journal_accounts WHERE project_id = $1 AND kind = 'customer_credit'`, projectID).Scan(&b.BalanceMicros, &b.ReservedMicros)
	if errors.Is(err, pgx.ErrNoRows) {
		return Balance{}, nil
	}
	b.AvailableMicros = b.BalanceMicros - b.ReservedMicros
	return b, err
}

// Reserve holds micros for one request before any provider is called. The
// conditional update and the reservation row commit together, so holds
// survive a crash and concurrent requests can never spend the same funds.
func (s *Store) Reserve(ctx context.Context, projectID, requestID string, micros int64, ttl time.Duration) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		acct, err := account(ctx, tx, projectID, "customer_credit")
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE journal_accounts SET reserved_micros = reserved_micros + $2
			WHERE id = $1 AND balance_micros - reserved_micros >= $2`, acct, micros)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrInsufficientFunds
		}
		_, err = tx.Exec(ctx, `INSERT INTO credit_reservations (inference_request_id, account_id, amount_micros, status, expires_at)
			VALUES ($1, $2, $3, 'active', now() + $4::interval)`, requestID, acct, micros, fmt.Sprintf("%d seconds", int(ttl.Seconds())))
		return err
	})
}

// resolve finishes an active hold exactly once; a replay finds it resolved
// and changes nothing.
func (s *Store) resolve(ctx context.Context, requestID string, apply func(tx pgx.Tx, acct string, hold int64) (status string, settled *int64, note string, err error)) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var acct, status string
		var hold int64
		err := tx.QueryRow(ctx, `SELECT account_id, amount_micros, status FROM credit_reservations WHERE inference_request_id = $1 FOR UPDATE`, requestID).Scan(&acct, &hold, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil || status != "active" {
			return err
		}
		next, settled, note, err := apply(tx, acct, hold)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE journal_accounts SET reserved_micros = reserved_micros - $2 WHERE id = $1`, acct, hold); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE credit_reservations SET status = $2, settled_micros = $3, note = $4, resolved_at = now() WHERE inference_request_id = $1`, requestID, next, settled, nullable(note))
		return err
	})
}

// Settle charges a request's actual cost and frees the rest of its hold, in
// one transaction. A cost above the hold (a bad estimate) charges the hold
// and leaves the request for review rather than overdrawing the account.
func (s *Store) Settle(ctx context.Context, requestID string, cost int64) error {
	return s.resolve(ctx, requestID, func(tx pgx.Tx, acct string, hold int64) (string, *int64, string, error) {
		status, note, charge := "settled", "", cost
		if cost > hold {
			status, note, charge = "pending_review", fmt.Sprintf("cost %d exceeded hold %d", cost, hold), hold
		}
		if charge > 0 {
			revenue, err := account(ctx, tx, "", "platform_revenue")
			if err != nil {
				return "", nil, "", err
			}
			if err := post(ctx, tx, "inference_settle", requestID, "", "", map[string]int64{acct: -charge, revenue: charge}); err != nil {
				return "", nil, "", err
			}
		}
		return status, &charge, note, nil
	})
}

// Release frees a hold for a request that certainly cost nothing.
func (s *Store) Release(ctx context.Context, requestID string) error {
	return s.resolve(ctx, requestID, func(pgx.Tx, string, int64) (string, *int64, string, error) {
		return "released", nil, "", nil
	})
}

// Hold marks a request whose cost is unknown (a cut stream, missing usage).
// Its hold stays reserved until someone resolves it: unknown is not zero.
func (s *Store) Hold(ctx context.Context, requestID, note string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE credit_reservations SET status = 'pending_review', note = $2 WHERE inference_request_id = $1 AND status = 'active'`, requestID, note)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// Claim records an idempotency key for a project's request. Reusing a key
// returns ErrDuplicate with the original request id, whether or not the body
// matches: without stored responses, a replay could only charge twice.
func (s *Store) Claim(ctx context.Context, projectID, key string, body []byte, requestID string) (string, bool, error) {
	sum := sha256.Sum256(body)
	tag, err := s.pool.Exec(ctx, `INSERT INTO idempotency_keys (project_id, key, fingerprint, inference_request_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, projectID, key, sum[:], requestID)
	if err != nil || tag.RowsAffected() == 1 {
		return "", false, err
	}
	var original string
	var fingerprint []byte
	if err := s.pool.QueryRow(ctx, `SELECT inference_request_id, fingerprint FROM idempotency_keys WHERE project_id = $1 AND key = $2`, projectID, key).Scan(&original, &fingerprint); err != nil {
		return "", false, err
	}
	return original, string(fingerprint) == string(sum[:]), ErrDuplicate
}

// ExpireHolds resolves active holds past their expiry, which only happens
// when the gateway died mid-request. Rows are claimed with SKIP LOCKED, so
// several workers can run. A request that failed is released; one with a
// known cost is settled; anything else waits for review.
func (s *Store) ExpireHolds(ctx context.Context, limit int) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.inference_request_id, q.status, u.cost_micros
		FROM credit_reservations r
		LEFT JOIN inference_requests q ON q.id = r.inference_request_id
		LEFT JOIN request_usage u ON u.inference_request_id = r.inference_request_id
		WHERE r.status = 'active' AND r.expires_at < now()
		ORDER BY r.expires_at LIMIT $1 FOR UPDATE OF r SKIP LOCKED`, limit)
	if err != nil {
		return 0, err
	}
	type expired struct {
		id     string
		status *string
		cost   *int64
	}
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (expired, error) {
		var e expired
		return e, r.Scan(&e.id, &e.status, &e.cost)
	})
	if err != nil {
		return 0, err
	}
	for _, e := range found {
		switch {
		case e.status != nil && *e.status == "failed":
			err = s.Release(ctx, e.id)
		case e.status != nil && *e.status == "ok" && e.cost != nil:
			err = s.Settle(ctx, e.id, *e.cost)
		default:
			err = s.Hold(ctx, e.id, "hold expired with unknown cost")
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return 0, err
		}
	}
	return len(found), nil
}

// LedgerEntry is one line of a project's statement.
type LedgerEntry struct {
	Kind      string    `json:"kind"`
	Request   string    `json:"inference_request_id,omitempty"`
	Reference string    `json:"reference,omitempty"`
	Micros    int64     `json:"amount_micros"`
	At        time.Time `json:"at"`
}

func (s *Store) Ledger(ctx context.Context, projectID string, limit int) ([]LedgerEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT t.kind, COALESCE(t.inference_request_id, ''), COALESCE(t.external_reference, ''), e.amount_micros, e.created_at
		FROM journal_entries e JOIN journal_transactions t ON t.id = e.journal_transaction_id
		JOIN journal_accounts a ON a.id = e.journal_account_id
		WHERE a.project_id = $1 AND a.kind = 'customer_credit' ORDER BY e.created_at DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (LedgerEntry, error) {
		var l LedgerEntry
		return l, r.Scan(&l.Kind, &l.Request, &l.Reference, &l.Micros, &l.At)
	})
}

// Unbalanced lists transactions whose entries do not sum to zero; it must
// always be empty.
func (s *Store) Unbalanced(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT journal_transaction_id::text FROM journal_entries GROUP BY 1 HAVING sum(amount_micros) <> 0`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
