package billing

import (
	"example.com/minirouter/internal/core"
	"testing"
)

func TestCostMicros(t *testing.T) {
	p := &core.Price{InputMicrosPerMillion: 3_000_000, OutputMicrosPerMillion: 15_000_000}
	cost, ok := CostMicros(core.Usage{InputTokens: 10_000, OutputTokens: 2_000}, p)
	if !ok || cost != 60_000 {
		t.Fatalf("want 60000 microUSD, got %d %v", cost, ok)
	}
	if _, known := CostMicros(core.Usage{}, nil); known {
		t.Fatal("nil price must mean unknown")
	}
}
func TestCachedInputNotDoubleBilled(t *testing.T) {
	p := &core.Price{InputMicrosPerMillion: 1_000_000, CacheReadMicrosPerMillion: 100_000}
	cost, ok := CostMicros(core.Usage{InputTokens: 1000, CachedInput: 900}, p)
	if !ok || cost != 190 {
		t.Fatalf("want 190 microUSD, got %d", cost)
	}
}
