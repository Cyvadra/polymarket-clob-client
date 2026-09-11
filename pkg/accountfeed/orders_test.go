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
	intent         store.OrderIntentRecord
	updated        statemachine.State
	matchedShares  string
	observedEvents []statemachine.Event
	dedupSeen      map[string]struct{}
	averagePrice   string
}

type recordedPublisher struct {
	results      []protocol.ExecutionOpenResult
	closeResults []protocol.ExecutionCloseResult
	orderEvents  []protocol.ExecutionOrderEvent
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
	case protocol.SubjectExecutionOrderEvent:
		if event, ok := value.(protocol.ExecutionOrderEvent); ok {
			p.orderEvents = append(p.orderEvents, event)
		}
	}
	return nil
}

func (s *fakeOrderStore) PersistSignedOrder(context.Context, store.SignedOrderRecord) error {
	return nil
}
func (s *fakeOrderStore) UpdateIntentStatus(_ context.Context, intentID, status string) error {
	if s.intent.IntentID == intentID {
		s.intent.Status = statemachine.State(status)
		return nil
	}
	if s.order.IntentID == intentID {
		s.intent = store.OrderIntentRecord{IntentID: intentID, Kind: store.IntentOpen, Status: statemachine.State(status)}
		return nil
	}
	return store.ErrNotFound
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
func (s *fakeOrderStore) OrderAveragePrice(context.Context, string) (string, error) {
	return s.averagePrice, nil
}
func (s *fakeOrderStore) Intent(_ context.Context, intentID string) (store.OrderIntentRecord, error) {
	if s.intent.IntentID == intentID {
		return s.intent, nil
	}
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
	if err := PublishTerminalResult(publisher, intent, order, "canceled", "", time.Unix(1, 0)); err != nil {
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
	if err := PublishTerminalResult(publisher, intent, order, "canceled", "", time.Unix(1, 0)); err != nil {
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
	if err := PublishTerminalResult(publisher, intent, order, "filled", "", time.Unix(1, 0)); err != nil {
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
	if err := PublishTerminalResult(publisher, intent, order, "filled", "", time.Unix(1, 0)); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	if len(publisher.results) != 0 {
		t.Fatalf("expected no open result for a close intent, got %+v", publisher.results)
	}
	if len(publisher.closeResults) != 1 || publisher.closeResults[0].Status != protocol.ResultSucceeded || publisher.closeResults[0].AssetID != "token" {
		t.Fatalf("expected a successful close result carrying the asset ID, got %+v", publisher.closeResults)
	}
}

func TestPublishTerminalResultSkipsSupersededCloseIntent(t *testing.T) {
	publisher := &recordedPublisher{}
	intent := store.OrderIntentRecord{IntentID: "intent-1", Kind: store.IntentClose, Status: store.IntentStatusSuperseded, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideSell}
	order := store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: store.StrategyChildSequence, State: statemachine.StateCanceled, MatchedShares: "0"}
	if err := PublishTerminalResult(publisher, intent, order, "canceled", "", time.Unix(1, 0)); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	if len(publisher.closeResults) != 0 {
		t.Fatalf("expected no close result for a superseded close intent, got %+v", publisher.closeResults)
	}
}

func TestPublishTerminalResultSkipsCloseInternalChild(t *testing.T) {
	publisher := &recordedPublisher{}
	intent := store.OrderIntentRecord{IntentID: "intent-1", Kind: store.IntentClose, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideSell}
	order := store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: store.StrategyChildSequence + 1, State: statemachine.StateFilled, MatchedShares: "2"}
	if err := PublishTerminalResult(publisher, intent, order, "filled", "", time.Unix(1, 0)); err != nil {
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

func TestAveragePriceFallsBackToLimitOnlyForRestingOrders(t *testing.T) {
	// No recorded fill price. A GTC order could only have filled by resting,
	// where the maker takes its own limit, so the limit is the exact price.
	resting := store.SignedOrderRecord{ExchangeOrderID: "o1", State: statemachine.StateFilled, MatchedShares: "3", Price: "0.55", OrderType: store.TimeInForceGTC}
	if got := AveragePrice(context.Background(), &fakeOrderStore{}, resting); got != "0.55" {
		t.Fatalf("expected the resting limit 0.55, got %q", got)
	}
	// A taker order (FAK) that crossed on arrival is priced by the executor
	// from the submit response, not here; no fallback.
	taker := resting
	taker.OrderType = store.TimeInForceFAK
	if got := AveragePrice(context.Background(), &fakeOrderStore{}, taker); got != "" {
		t.Fatalf("expected no fallback for a taker order, got %q", got)
	}
	// A recorded fill price always wins over the limit fallback.
	if got := AveragePrice(context.Background(), &fakeOrderStore{averagePrice: "0.5312"}, resting); got != "0.5312" {
		t.Fatalf("expected the recorded fill price, got %q", got)
	}
	// Nothing filled: no price at all.
	unfilled := resting
	unfilled.MatchedShares = "0"
	unfilled.State = statemachine.StateCanceled
	if got := AveragePrice(context.Background(), &fakeOrderStore{}, unfilled); got != "" {
		t.Fatalf("expected no price for an unfilled order, got %q", got)
	}
}

func TestOrderConsumerStampsOrderEventWithLaneTag(t *testing.T) {
	repository := &fakeOrderStore{
		order:  store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 2},
		intent: store.OrderIntentRecord{IntentID: "intent-1", Kind: store.IntentOpen, UniqueTag: "lane-xyz", ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy},
	}
	consumer, err := NewOrderConsumer(repository, time.Now)
	if err != nil {
		t.Fatalf("new order consumer: %v", err)
	}
	publisher := &recordedPublisher{}
	consumer.SetEventPublisher(publisher)
	if err := consumer.Consume(context.Background(), AccountOrderEvent{SchemaVersion: protocol.SchemaVersionV1, EventID: "event-1", ExchangeOrderID: "order-1", Status: "CANCELED", MatchedShares: "1"}); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(publisher.orderEvents) != 1 || publisher.orderEvents[0].UniqueTag != "lane-xyz" {
		t.Fatalf("expected the order event stamped with the lane tag, got %+v", publisher.orderEvents)
	}
}

func TestOrderConsumerPublishesUntaggedEventWhenIntentGone(t *testing.T) {
	// order-2's intent is not in the store, so the event still goes out (for
	// observers) but carries no tag and no terminal result follows.
	repository := &fakeOrderStore{order: store.SignedOrderRecord{IntentID: "intent-missing", ChildSequence: 1, ExchangeOrderID: "order-2", State: statemachine.StateLive, Revision: 2}}
	repository.intent = store.OrderIntentRecord{} // Intent() returns a fabricated open record for s.order.IntentID; force not-found
	consumer, err := NewOrderConsumer(&intentlessOrderStore{repository}, time.Now)
	if err != nil {
		t.Fatalf("new order consumer: %v", err)
	}
	publisher := &recordedPublisher{}
	consumer.SetEventPublisher(publisher)
	if err := consumer.Consume(context.Background(), AccountOrderEvent{SchemaVersion: protocol.SchemaVersionV1, EventID: "event-1", ExchangeOrderID: "order-2", Status: "CANCELED", MatchedShares: "0"}); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(publisher.orderEvents) != 1 || publisher.orderEvents[0].UniqueTag != "" {
		t.Fatalf("expected one untagged order event, got %+v", publisher.orderEvents)
	}
	if len(publisher.results) != 0 {
		t.Fatalf("expected no terminal result without an intent, got %+v", publisher.results)
	}
}

// intentlessOrderStore forces Intent to report ErrNotFound while delegating
// everything else, modelling an order whose intent row is gone.
type intentlessOrderStore struct{ *fakeOrderStore }

func (intentlessOrderStore) Intent(context.Context, string) (store.OrderIntentRecord, error) {
	return store.OrderIntentRecord{}, store.ErrNotFound
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
