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
	results      []protocol.ExecutionOpenResult
	closeResults []protocol.ExecutionCloseResult
}

func (p *recordedPublisher) PublishJSON(subject string, value any) error {
	switch subject {
	case protocol.SubjectExecutionOpenResult:
		if result, ok := value.(protocol.ExecutionOpenResult); ok {
			p.results = append(p.results, result)
		}
	case protocol.SubjectExecutionCloseResult:
		if result, ok := value.(protocol.ExecutionCloseResult); ok {
			p.closeResults = append(p.closeResults, result)
		}
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
	order.State = transition.To
	order.Revision++
	order.MatchedShares = matchedShares
	if exchangeOrderID != "" {
		order.ExchangeOrderID = exchangeOrderID
	}
	s.order = order
	return order, nil
}
func (s *fakeOrderStore) OrderByExchangeID(_ context.Context, exchangeOrderID string) (store.SignedOrderRecord, error) {
	if exchangeOrderID != s.order.ExchangeOrderID {
		return store.SignedOrderRecord{}, store.ErrNotFound
	}
	return s.order, nil
}
func (s *fakeOrderStore) Intent(_ context.Context, intentID string) (store.OrderIntentRecord, error) {
	if s.order.IntentID == intentID {
		return store.OrderIntentRecord{IntentID: intentID, Kind: store.IntentOpen}, nil
	}
	return store.OrderIntentRecord{}, store.ErrNotFound
}

func TestOrderConsumerTransitionsKnownOrder(t *testing.T) {
	repository := &fakeOrderStore{order: store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 2}}
	consumer, err := NewOrderConsumer(repository, time.Now)
	if err != nil {
		t.Fatalf("new order consumer: %v", err)
	}
	if err := consumer.Consume(context.Background(), AccountOrderEvent{SchemaVersion: protocol.SchemaVersionV1, EventID: "event-1", ExchangeOrderID: "order-1", Status: "CANCELED", MatchedShares: "1.25"}); err != nil {
		t.Fatalf("consume order event: %v", err)
	}
	if repository.updated != statemachine.StateCanceled {
		t.Fatalf("expected canceled state, got %s", repository.updated)
	}
	if repository.matchedShares != "1.25" {
		t.Fatalf("expected matched shares to be observed, got %q", repository.matchedShares)
	}
}

func TestPublishTerminalResultCanceledWithoutFillIsFailed(t *testing.T) {
	publisher := &recordedPublisher{}
	intent := store.OrderIntentRecord{IntentID: "intent-1", Kind: store.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy}
	order := store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: store.StrategyChildSequence, State: statemachine.StateCanceled, MatchedShares: "0"}
	if err := PublishTerminalResult(publisher, intent, order, "canceled", time.Unix(1, 0)); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	if len(publisher.results) != 1 || publisher.results[0].Status != protocol.ResultFailed {
		t.Fatalf("expected failed result, got %+v", publisher.results)
	}
}

func TestPublishTerminalResultCanceledWithFillIsSuccess(t *testing.T) {
	publisher := &recordedPublisher{}
	intent := store.OrderIntentRecord{IntentID: "intent-1", Kind: store.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy}
	order := store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: store.StrategyChildSequence, State: statemachine.StateCanceled, MatchedShares: "1.25"}
	if err := PublishTerminalResult(publisher, intent, order, "canceled", time.Unix(1, 0)); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	if len(publisher.results) != 1 || publisher.results[0].Status != protocol.ResultSucceeded {
		t.Fatalf("expected success result, got %+v", publisher.results)
	}
}

func TestPublishTerminalResultSkipsInternalChildren(t *testing.T) {
	publisher := &recordedPublisher{}
	intent := store.OrderIntentRecord{IntentID: "intent-1", Kind: store.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy}
	order := store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: store.StrategyChildSequence + 1, State: statemachine.StateFilled, MatchedShares: "2"}
	if err := PublishTerminalResult(publisher, intent, order, "filled", time.Unix(1, 0)); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	if len(publisher.results) != 0 {
		t.Fatalf("expected no result for internal child, got %+v", publisher.results)
	}
}

func TestPublishTerminalResultEmitsCloseResultForCloseIntent(t *testing.T) {
	publisher := &recordedPublisher{}
	intent := store.OrderIntentRecord{IntentID: "intent-1", Kind: store.IntentClose, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideSell}
	order := store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: store.StrategyChildSequence, State: statemachine.StateFilled, MatchedShares: "2"}
	if err := PublishTerminalResult(publisher, intent, order, "filled", time.Unix(1, 0)); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	if len(publisher.results) != 0 {
		t.Fatalf("expected no open result for a close intent, got %+v", publisher.results)
	}
	if len(publisher.closeResults) != 1 || publisher.closeResults[0].Status != protocol.ResultSucceeded || publisher.closeResults[0].AssetID != "token" {
		t.Fatalf("expected a successful close result carrying the asset ID, got %+v", publisher.closeResults)
	}
}

func TestPublishTerminalResultSkipsCloseInternalChild(t *testing.T) {
	publisher := &recordedPublisher{}
	intent := store.OrderIntentRecord{IntentID: "intent-1", Kind: store.IntentClose, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideSell}
	order := store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: store.StrategyChildSequence + 1, State: statemachine.StateFilled, MatchedShares: "2"}
	if err := PublishTerminalResult(publisher, intent, order, "filled", time.Unix(1, 0)); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	if len(publisher.closeResults) != 0 {
		t.Fatalf("expected no close result for a force-close exit, got %+v", publisher.closeResults)
	}
}

func TestOrderConsumerClosesPartiallyMatchedImmediateOrder(t *testing.T) {
	repository := &fakeOrderStore{order: store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 2, RequestedShares: "5", OrderType: store.TimeInForceFAK}}
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

func TestOrderConsumerKeepsPartiallyMatchedRestingOrderOpen(t *testing.T) {
	repository := &fakeOrderStore{order: store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 2, RequestedShares: "5", OrderType: store.TimeInForceGTC}}
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
