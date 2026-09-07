package accountfeed

import (
	"context"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
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

type recordedPublisher struct {
	acks []protocol.ExecutionIntentAck
}

func (p *recordedPublisher) PublishJSON(subject string, value any) error {
	if subject != protocol.SubjectExecutionIntentAck {
		return nil
	}
	ack, ok := value.(protocol.ExecutionIntentAck)
	if ok {
		p.acks = append(p.acks, ack)
	}
	return nil
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

func TestOrderConsumerTransitionsKnownOrder(t *testing.T) {
	repository := &fakeOrderStore{order: store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 2}}
	consumer, err := NewOrderConsumer(repository, time.Now)
	if err != nil {
		t.Fatalf("new order consumer: %v", err)
	}
	err = consumer.Consume(context.Background(), AccountOrderEvent{SchemaVersion: protocol.SchemaVersionV1, EventID: "event-1", ExchangeOrderID: "order-1", Status: "CANCELED", MatchedShares: "1.25"})
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

func TestPublishTerminalAckCanceledWithoutFillIsExpired(t *testing.T) {
	for _, matchedShares := range []string{"0", "0.000000000000000000"} {
		publisher := &recordedPublisher{}
		order := store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: store.StrategyChildSequence, State: statemachine.StateCanceled, MatchedShares: matchedShares}
		if err := PublishTerminalAck(publisher, order, "canceled", time.Unix(1, 0)); err != nil {
			t.Fatalf("publish ack: %v", err)
		}
		if len(publisher.acks) != 1 || publisher.acks[0].Status != protocol.IntentExpired {
			t.Fatalf("expected expired ack for %q, got %+v", matchedShares, publisher.acks)
		}
	}
}

func TestPublishTerminalAckCanceledWithFillIsPartial(t *testing.T) {
	publisher := &recordedPublisher{}
	order := store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: store.StrategyChildSequence, State: statemachine.StateCanceled, MatchedShares: "1.25"}
	if err := PublishTerminalAck(publisher, order, "canceled", time.Unix(1, 0)); err != nil {
		t.Fatalf("publish ack: %v", err)
	}
	if len(publisher.acks) != 1 || publisher.acks[0].Status != protocol.IntentPartial {
		t.Fatalf("expected partial ack, got %+v", publisher.acks)
	}
}

// The strategy never asked for an internal child such as a force-close exit,
// so its outcome must not be reported as the intent's outcome.
func TestPublishTerminalAckSkipsInternalChildren(t *testing.T) {
	publisher := &recordedPublisher{}
	order := store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: store.StrategyChildSequence + 1, State: statemachine.StateFilled, MatchedShares: "2"}
	if err := PublishTerminalAck(publisher, order, "filled", time.Unix(1, 0)); err != nil {
		t.Fatalf("publish ack: %v", err)
	}
	if len(publisher.acks) != 0 {
		t.Fatalf("expected no intent ack for an internal child, got %+v", publisher.acks)
	}
}

// A FAK cannot rest, so a partial match ends it. Leaving it partially filled
// would hold its sell reservation open and block every later sell on the token.
func TestOrderConsumerClosesPartiallyMatchedImmediateOrder(t *testing.T) {
	repository := &fakeOrderStore{order: store.SignedOrderRecord{
		IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1",
		State: statemachine.StateLive, Revision: 2, RequestedShares: "5", OrderType: store.TimeInForceFAK,
	}}
	consumer, err := NewOrderConsumer(repository, time.Now)
	if err != nil {
		t.Fatalf("new order consumer: %v", err)
	}
	if err := consumer.Consume(context.Background(), AccountOrderEvent{SchemaVersion: protocol.SchemaVersionV1, EventID: "event-1", ExchangeOrderID: "order-1", Status: "MATCHED", MatchedShares: "2"}); err != nil {
		t.Fatalf("consume order event: %v", err)
	}
	if repository.updated != statemachine.StateCanceled {
		t.Fatalf("expected the unmatched remainder to close the order, got %s", repository.updated)
	}
}

// A resting order with the same observation is still working.
func TestOrderConsumerKeepsPartiallyMatchedRestingOrderOpen(t *testing.T) {
	repository := &fakeOrderStore{order: store.SignedOrderRecord{
		IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1",
		State: statemachine.StateLive, Revision: 2, RequestedShares: "5", OrderType: store.TimeInForceGTC,
	}}
	consumer, err := NewOrderConsumer(repository, time.Now)
	if err != nil {
		t.Fatalf("new order consumer: %v", err)
	}
	if err := consumer.Consume(context.Background(), AccountOrderEvent{SchemaVersion: protocol.SchemaVersionV1, EventID: "event-1", ExchangeOrderID: "order-1", Status: "MATCHED", MatchedShares: "2"}); err != nil {
		t.Fatalf("consume order event: %v", err)
	}
	if repository.updated != statemachine.StatePartiallyFilled {
		t.Fatalf("expected a resting order to stay open, got %s", repository.updated)
	}
}

func TestOrderConsumerIgnoresUnknownOrder(t *testing.T) {
	consumer, err := NewOrderConsumer(&fakeOrderStore{}, time.Now)
	if err != nil {
		t.Fatalf("new order consumer: %v", err)
	}
	if err := consumer.Consume(context.Background(), AccountOrderEvent{EventID: "event-1", ExchangeOrderID: "missing", Status: "LIVE"}); err != nil {
		t.Fatalf("unknown order should be deferred to reconciliation: %v", err)
	}
}

func TestOrderConsumerDuplicateObservationIsIdempotent(t *testing.T) {
	repository := &fakeOrderStore{order: store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 2}}
	consumer, err := NewOrderConsumer(repository, time.Now)
	if err != nil {
		t.Fatalf("new order consumer: %v", err)
	}
	event := AccountOrderEvent{EventID: "event-1", ExchangeOrderID: "order-1", Status: "CANCELED", MatchedShares: "1.25"}
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
