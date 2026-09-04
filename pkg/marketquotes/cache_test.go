package marketquotes

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/contracts"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

type fakeSubscriber struct {
	subject string
	handler natsbus.Handler
}

func (s *fakeSubscriber) Subscribe(subject string, handler natsbus.Handler) error {
	s.subject, s.handler = subject, handler
	return nil
}

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
	stored, ok := cache.Get("market-1")
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

func TestSubscribeDecodesMarketQuotes(t *testing.T) {
	cache := New()
	subscriber := &fakeSubscriber{}
	if err := Subscribe(subscriber, cache); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if subscriber.subject != contracts.SubjectPMMMarketQuotes || subscriber.handler == nil {
		t.Fatalf("unexpected subscription: %+v", subscriber)
	}
	payload, err := json.Marshal(testQuotes(time.Unix(20, 0)))
	if err != nil {
		t.Fatalf("marshal quotes: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("deliver quotes: %v", err)
	}
	if _, ok := cache.Get("market-1"); !ok {
		t.Fatal("quote was not cached")
	}
}

func testQuotes(at time.Time) contracts.MarketQuotes {
	return contracts.MarketQuotes{
		MarketID: "market-1", At: at,
		Up:   contracts.MarketQuote{Bid: 0.48, Ask: 0.52, Mid: 0.5, Timestamp: at},
		Down: contracts.MarketQuote{Bid: 0.47, Ask: 0.53, Mid: 0.5, Timestamp: at},
	}
}
