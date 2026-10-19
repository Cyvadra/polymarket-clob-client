package nats

import (
	"context"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
)

// A levels message as pmm's collector publishes it, with the depth figures
// the cache ignores and a Down side that never changed since the flush.
func TestSubscribeLevelsCachesTheBookTop(t *testing.T) {
	subscriber := &fakeSubscriber{}
	cache := marketquotes.New()
	if err := SubscribeLevels(subscriber, cache); err != nil {
		t.Fatalf("subscribe levels: %v", err)
	}
	if subscriber.subject != protocol.SubjectMarketLevels || subscriber.handler == nil {
		t.Fatalf("subscription=%+v", subscriber)
	}
	payload := []byte(`{"condition_id":"condition","at":"2026-10-19T10:00:00.500Z",
		"up":{"asset_id":"up-token","bid":0.72,"ask":0.73,"bid_usd_5c":120.5,"ask_usd_5c":80,"bid_levels":3,"ask_levels":2,"updated_at":"2026-10-19T10:00:00.250Z"},
		"down":{"asset_id":"down-token","bid":0.27,"ask":0.28,"updated_at":"0001-01-01T00:00:00Z"}}`)
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("handle levels: %v", err)
	}
	snapshot, ok := cache.Get("condition")
	if !ok {
		t.Fatal("book not cached")
	}
	up := snapshot.Up
	if up.AssetID != "up-token" || up.Bid != 0.72 || up.Ask != 0.73 || up.Mid != 0.725 || !up.Timestamp.Equal(time.Date(2026, 10, 19, 10, 0, 0, 250e6, time.UTC)) {
		t.Fatalf("up=%+v", up)
	}
	down := snapshot.Down
	if down.AssetID != "down-token" || down.Bid != 0.27 || down.Ask != 0.28 || down.Mid != 0.275 || !down.Timestamp.Equal(snapshot.At) {
		t.Fatalf("down side=%+v", down)
	}
}

// A book with an empty side is not stored, so the market's book ages out
// and the quote decides.
func TestSubscribeLevelsSkipsAOneSidedBook(t *testing.T) {
	subscriber := &fakeSubscriber{}
	cache := marketquotes.New()
	if err := SubscribeLevels(subscriber, cache); err != nil {
		t.Fatalf("subscribe levels: %v", err)
	}
	payload := []byte(`{"condition_id":"condition","at":"2026-10-19T10:00:00Z",
		"up":{"asset_id":"up-token","bid":0.98,"ask":0},
		"down":{"asset_id":"down-token","bid":0.01,"ask":0.02}}`)
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("handle levels: %v", err)
	}
	if _, ok := cache.Get("condition"); ok {
		t.Fatal("a one-sided book was cached")
	}
}

func TestSubscribeLevelsRejectsAMessageWithoutAMarket(t *testing.T) {
	subscriber := &fakeSubscriber{}
	cache := marketquotes.New()
	if err := SubscribeLevels(subscriber, cache); err != nil {
		t.Fatalf("subscribe levels: %v", err)
	}
	if err := subscriber.handler(context.Background(), []byte(`{"at":"2026-10-19T10:00:00Z","up":{"asset_id":"u","bid":0.4,"ask":0.5},"down":{"asset_id":"d","bid":0.5,"ask":0.6}}`)); err == nil {
		t.Fatal("expected a levels message without a condition refused")
	}
	if _, ok := cache.Get(""); ok {
		t.Fatal("invalid levels were cached")
	}
}
