package accountfeed

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// latePriceStore answers the fill-weighted price only once the fills have
// "landed", which a test decides, so a result published before then is the
// unpriced one the client reported.
type latePriceStore struct {
	*fakeOrderStore
	mu    sync.Mutex
	price string
}

func (s *latePriceStore) OrderAveragePrice(context.Context, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.price, nil
}

func (s *latePriceStore) landFills(price string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.price = price
}

type syncPublisher struct {
	mu      sync.Mutex
	results []protocol.ExecutionOpenResult
}

func (p *syncPublisher) PublishJSON(subject string, value any) error {
	if subject != protocol.SubjectExecutionOpenResult {
		return nil
	}
	result, ok := value.(protocol.ExecutionOpenResult)
	if !ok {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.results = append(p.results, result)
	return nil
}

func (p *syncPublisher) snapshot() []protocol.ExecutionOpenResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]protocol.ExecutionOpenResult(nil), p.results...)
}

func (p *syncPublisher) await(t *testing.T, count int) []protocol.ExecutionOpenResult {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if results := p.snapshot(); len(results) >= count {
			return results
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("expected %d open results, got %+v", count, p.snapshot())
	return nil
}

func matchedOrderConsumer(t *testing.T, wait time.Duration) (*OrderConsumer, *latePriceStore, *syncPublisher) {
	t.Helper()
	repository := &latePriceStore{fakeOrderStore: &fakeOrderStore{order: store.SignedOrderRecord{
		IntentID: "intent-1", ChildSequence: store.StrategyChildSequence, ExchangeOrderID: "order-1",
		State: statemachine.StateLive, Revision: 2, RequestedShares: "7.1", MatchedShares: "0",
		Price: "0.69", OrderType: store.TimeInForceFAK,
	}}}
	consumer, err := NewOrderConsumer(repository, time.Now)
	if err != nil {
		t.Fatalf("new order consumer: %v", err)
	}
	publisher := &syncPublisher{}
	consumer.SetEventPublisher(publisher)
	consumer.SetPriceWait(wait)
	return consumer, repository, publisher
}

func matchedEvent(id string) AccountOrderEvent {
	return AccountOrderEvent{SchemaVersion: protocol.SchemaVersionV1, EventID: id, ExchangeOrderID: "order-1", Status: "MATCHED", MatchedShares: "7.1"}
}

func TestOrderConsumerWaitsForLateFillPrice(t *testing.T) {
	consumer, repository, publisher := matchedOrderConsumer(t, 2*time.Second)
	if err := consumer.Consume(context.Background(), matchedEvent("event-1")); err != nil {
		t.Fatalf("consume order event: %v", err)
	}
	if results := publisher.snapshot(); len(results) != 0 {
		t.Fatalf("expected the result to wait for its price, got %+v", results)
	}
	repository.landFills("0.69")
	results := publisher.await(t, 1)
	if len(results) != 1 || results[0].AveragePrice != 0.69 || results[0].FilledShares != 7.1 {
		t.Fatalf("expected one result priced at 0.69, got %+v", results)
	}
}

func TestOrderConsumerFallsBackToOrderPriceWhenFillsNeverLand(t *testing.T) {
	consumer, _, publisher := matchedOrderConsumer(t, 150*time.Millisecond)
	if err := consumer.Consume(context.Background(), matchedEvent("event-1")); err != nil {
		t.Fatalf("consume order event: %v", err)
	}
	results := publisher.await(t, 1)
	if len(results) != 1 || results[0].AveragePrice != 0.69 {
		t.Fatalf("expected the order's own price as the fallback, got %+v", results)
	}
}

func TestOrderConsumerPublishesPriceImmediatelyWhenFillsAlreadyLanded(t *testing.T) {
	consumer, repository, publisher := matchedOrderConsumer(t, 2*time.Second)
	repository.landFills("0.62")
	if err := consumer.Consume(context.Background(), matchedEvent("event-1")); err != nil {
		t.Fatalf("consume order event: %v", err)
	}
	results := publisher.snapshot()
	if len(results) != 1 || results[0].AveragePrice != 0.62 {
		t.Fatalf("expected an immediately priced result, got %+v", results)
	}
}

func TestOrderConsumerEmitsOneResultForRepeatedTerminalEvents(t *testing.T) {
	consumer, repository, publisher := matchedOrderConsumer(t, 2*time.Second)
	repository.landFills("0.06")
	for _, id := range []string{"event-1", "event-2", "event-3", "event-4"} {
		if err := consumer.Consume(context.Background(), matchedEvent(id)); err != nil {
			t.Fatalf("consume %s: %v", id, err)
		}
	}
	if results := publisher.snapshot(); len(results) != 1 {
		t.Fatalf("expected a single terminal result per order, got %+v", results)
	}
}
