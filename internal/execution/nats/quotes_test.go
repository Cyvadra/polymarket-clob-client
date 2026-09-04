package nats

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
)

func TestSubscribeQuotesDecodesAndCachesSnapshot(t *testing.T) {
	subscriber := &fakeSubscriber{}
	cache := marketquotes.New()
	if err := SubscribeQuotes(subscriber, cache); err != nil {
		t.Fatalf("subscribe quotes: %v", err)
	}
	if subscriber.subject != protocol.SubjectMarketQuotes || subscriber.handler == nil {
		t.Fatalf("subscription=%+v", subscriber)
	}
	now := time.Unix(100, 0).UTC()
	payload, err := json.Marshal(marketquotes.Snapshot{ConditionID: " condition ", At: now, Up: marketquotes.Quote{AssetID: " up-token ", Bid: 0.42, Ask: 0.44, Mid: 0.43, Timestamp: now}, Down: marketquotes.Quote{AssetID: "down-token", Bid: 0.56, Ask: 0.58, Mid: 0.57, Timestamp: now}})
	if err != nil {
		t.Fatalf("marshal quote: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("handle quote: %v", err)
	}
	snapshot, ok := cache.Get("condition")
	if !ok || snapshot.Up.AssetID != "up-token" || snapshot.Up.Bid != 0.42 {
		t.Fatalf("cached snapshot=%+v ok=%v", snapshot, ok)
	}
}

func TestSubscribeQuotesRejectsInvalidSnapshot(t *testing.T) {
	subscriber := &fakeSubscriber{}
	cache := marketquotes.New()
	if err := SubscribeQuotes(subscriber, cache); err != nil {
		t.Fatalf("subscribe quotes: %v", err)
	}
	payload, err := json.Marshal(marketquotes.Snapshot{ConditionID: "condition"})
	if err != nil {
		t.Fatalf("marshal quote: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err == nil {
		t.Fatal("expected invalid snapshot error")
	}
	if _, ok := cache.Get("condition"); ok {
		t.Fatal("invalid snapshot was cached")
	}
}
