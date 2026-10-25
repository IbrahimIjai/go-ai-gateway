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
func TestParseUSD(t *testing.T) {
	for raw, want := range map[string]int64{"3": 3_000_000, "0.40": 400_000, "1.250000": 1_250_000, "0.000001": 1} {
		if got, err := ParseUSD(raw); err != nil || got != want {
			t.Errorf("%q: want %d, got %d %v", raw, want, got, err)
		}
	}
	for _, bad := range []string{"", "1.", ".5", "1.2345678", "-1", "1e3", "abc", "1000000"} {
		if _, err := ParseUSD(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}
func TestLongContextPricing(t *testing.T) {
	p := &core.Price{InputMicrosPerMillion: 1_000_000, LongContextThreshold: 200_000, LongContext: &core.Price{InputMicrosPerMillion: 2_000_000}}
	if c, _ := CostMicros(core.Usage{InputTokens: 200_000}, p); c != 200_000 {
		t.Fatalf("at threshold: want base rate, got %d", c)
	}
	if c, _ := CostMicros(core.Usage{InputTokens: 200_001}, p); c != 400_002 {
		t.Fatalf("above threshold: want long-context rate on the whole request, got %d", c)
	}
}
