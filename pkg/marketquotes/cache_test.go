package marketquotes

import (
	"testing"
	"time"
)

func TestCacheKeepsLatestMarketQuote(t *testing.T) {
	cache := New()
	latest := testQuotes(time.Unix(20, 0))
	if err := cache.Put(latest); err != nil {
		t.Fatalf("put latest: %v", err)
	}
	stale := testQuotes(time.Unix(10, 0))
	stale.Up.Bid = 0.1
	if err := cache.Put(stale); err != nil {
		t.Fatalf("put stale: %v", err)
	}
	stored, ok := cache.Get("condition-1")
	if !ok || !stored.At.Equal(latest.At) || stored.Up.Bid != latest.Up.Bid {
		t.Fatalf("stored quotes = %+v, exists=%v", stored, ok)
	}
}

func TestCacheRejectsInvalidQuotes(t *testing.T) {
	quotes := testQuotes(time.Unix(20, 0))
	quotes.Up.Mid = 0.9
	if err := New().Put(quotes); err == nil {
		t.Fatal("expected invalid mid to be rejected")
	}
}

func testQuotes(at time.Time) Snapshot {
	return Snapshot{
		ConditionID: "condition-1", At: at,
		Up:   Quote{AssetID: "up-token", Bid: 0.48, Ask: 0.52, Mid: 0.5, Timestamp: at},
		Down: Quote{AssetID: "down-token", Bid: 0.47, Ask: 0.53, Mid: 0.5, Timestamp: at},
	}
}
