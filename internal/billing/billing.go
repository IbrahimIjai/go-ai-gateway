// Package billing provides integer token costing and a concurrency-safe
// demonstration usage journal. It is NOT a production accounting ledger.
package billing

import (
	"fmt"
	"math/big"
	"sync"
	"time"

	"example.com/minirouter/internal/core"
)

// CostMicros rounds to the nearest microUSD. Cached tokens are subsets of
// InputTokens; output thinking tokens must already be included in OutputTokens.
// A nil price means unknown and MUST NOT be treated as free.
func CostMicros(u core.Usage, p *core.Price) (int64, bool) {
	if p == nil {
		return 0, false
	}
	standard := u.InputTokens - u.CachedInput - u.CacheWrite
	if standard < 0 || u.OutputTokens < 0 || u.CachedInput < 0 || u.CacheWrite < 0 {
		return 0, false
	}
	n := new(big.Int)
	n.Add(n, new(big.Int).Mul(big.NewInt(standard), big.NewInt(p.InputMicrosPerMillion)))
	n.Add(n, new(big.Int).Mul(big.NewInt(u.CachedInput), big.NewInt(p.CacheReadMicrosPerMillion)))
	n.Add(n, new(big.Int).Mul(big.NewInt(u.CacheWrite), big.NewInt(p.CacheWriteMicrosPerMillion)))
	n.Add(n, new(big.Int).Mul(big.NewInt(u.OutputTokens), big.NewInt(p.OutputMicrosPerMillion)))
	n.Add(n, big.NewInt(500_000))
	n.Div(n, big.NewInt(1_000_000))
	if !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}
func FormatUSD(micros int64) string {
	return fmt.Sprintf("%d.%06d", micros/1_000_000, micros%1_000_000)
}

type Store struct {
	mu       sync.Mutex
	records  []core.UsageRecord
	total    int64
	priced   int64
	count    int64
	unpriced int64
}

func NewStore() *Store { return &Store{records: make([]core.UsageRecord, 0, 128)} }
func (s *Store) Add(r core.UsageRecord, m core.Model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Time.IsZero() {
		r.Time = time.Now().UTC()
	}
	if r.Usage != nil && r.Status == "ok" {
		if n, known := CostMicros(*r.Usage, m.Price); known {
			s.total += n
			s.priced++
			value := FormatUSD(n)
			r.CostUSD = &value
		} else {
			s.unpriced++
		}
	}
	s.count++
	// Bound the demo store. Real deployments need an immutable DB ledger.
	if len(s.records) >= 1000 {
		copy(s.records, s.records[1:])
		s.records[len(s.records)-1] = r
	} else {
		s.records = append(s.records, r)
	}
}

type Summary struct {
	Requests         int64              `json:"requests"`
	PricedRequests   int64              `json:"priced_requests"`
	UnpricedRequests int64              `json:"unpriced_requests"`
	KnownTotalUSD    string             `json:"known_total_usd"`
	Recent           []core.UsageRecord `json:"recent"`
	Note             string             `json:"note"`
}

func (s *Store) Summary() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	recent := make([]core.UsageRecord, 0, 25)
	for i := len(s.records) - 1; i >= 0 && len(recent) < 25; i-- {
		recent = append(recent, s.records[i])
	}
	return Summary{Requests: s.count, PricedRequests: s.priced, UnpricedRequests: s.unpriced, KnownTotalUSD: FormatUSD(s.total), Recent: recent, Note: "In-memory demo usage only. No charges, credit reservations or durable reconciliation."}
}
