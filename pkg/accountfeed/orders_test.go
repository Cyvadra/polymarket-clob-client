package accountfeed

import (
	"context"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/contracts"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeOrderStore struct {
	order          store.SignedOrderRecord
	updated        statemachine.State
	matchedShares  string
	observedEvents []statemachine.Event
	dedupSeen      map[string]struct{}
}

func (s *fakeOrderStore) PersistSignedOrder(context.Context, store.SignedOrderRecord) error {
	return nil
}
func (s *fakeOrderStore) TransitionOrder(_ context.Context, order store.SignedOrderRecord, event statemachine.Event, matchedShares, exchangeOrderID, _ string) (store.SignedOrderRecord, error) {
	transition, _, err := statemachine.Apply(order.State, event)
	if err != nil {
		return store.SignedOrderRecord{}, err
	}
	if transition.To == order.State && (matchedShares == "" || matchedShares == order.MatchedShares) {
		return order, nil
	}
	s.updated = transition.To
	s.matchedShares = matchedShares
	s.observedEvents = append(s.observedEvents, event)
	s.order.State = transition.To
	s.order.Revision = order.Revision + 1
	s.order.MatchedShares = matchedShares
	if exchangeOrderID != "" {
		s.order.ExchangeOrderID = exchangeOrderID
	}
	return s.order, nil
}
func (s *fakeOrderStore) OrderByExchangeID(_ context.Context, exchangeOrderID string) (store.SignedOrderRecord, error) {
	if exchangeOrderID != s.order.ExchangeOrderID {
		return store.SignedOrderRecord{}, store.ErrNotFound
	}
	return s.order, nil
}
func (s *fakeOrderStore) OrderByIntent(context.Context, string, int) (store.SignedOrderRecord, error) {
	return store.SignedOrderRecord{}, store.ErrNotFound
}
func (s *fakeOrderStore) OpenOrders(context.Context) ([]store.SignedOrderRecord, error) {
	return nil, nil
}

func TestOrderConsumerTransitionsKnownOrder(t *testing.T) {
	repository := &fakeOrderStore{order: store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 2}}
	consumer, err := NewOrderConsumer(repository, time.Now)
	if err != nil {
		t.Fatalf("new order consumer: %v", err)
	}
	err = consumer.Consume(context.Background(), contracts.AccountOrderEvent{SchemaVersion: contracts.SchemaVersionV1, EventID: "event-1", ExchangeOrderID: "order-1", Status: "CANCELED", MatchedShares: "1.25"})
	if err != nil {
		t.Fatalf("consume order event: %v", err)
	}
	if repository.updated != statemachine.StateCanceled {
		t.Fatalf("expected canceled state, got %s", repository.updated)
	}
	if repository.matchedShares != "1.25" {
		t.Fatalf("expected matched shares to be observed, got %q", repository.matchedShares)
	}
}

func TestOrderConsumerIgnoresUnknownOrder(t *testing.T) {
	consumer, err := NewOrderConsumer(&fakeOrderStore{}, time.Now)
	if err != nil {
		t.Fatalf("new order consumer: %v", err)
	}
	if err := consumer.Consume(context.Background(), contracts.AccountOrderEvent{EventID: "event-1", ExchangeOrderID: "missing", Status: "LIVE"}); err != nil {
		t.Fatalf("unknown order should be deferred to reconciliation: %v", err)
	}
}

func TestOrderConsumerDuplicateObservationIsIdempotent(t *testing.T) {
	repository := &fakeOrderStore{order: store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 2}}
	consumer, err := NewOrderConsumer(repository, time.Now)
	if err != nil {
		t.Fatalf("new order consumer: %v", err)
	}
	event := contracts.AccountOrderEvent{EventID: "event-1", ExchangeOrderID: "order-1", Status: "CANCELED", MatchedShares: "1.25"}
	if err := consumer.Consume(context.Background(), event); err != nil {
		t.Fatalf("consume first order event: %v", err)
	}
	if err := consumer.Consume(context.Background(), event); err != nil {
		t.Fatalf("consume duplicate order event: %v", err)
	}
	if len(repository.observedEvents) != 1 {
		t.Fatalf("expected one observed event, got %v", repository.observedEvents)
	}
}

func TestStatusEvent(t *testing.T) {
	event, ok := statemachine.EventForOrderStatus("partially_filled")
	if !ok {
		t.Fatalf("expected status to map to an event")
	}
	if event != statemachine.EventPartialFillObserved {
		t.Fatalf("expected partial fill observed event, got %s", event)
	}
}
