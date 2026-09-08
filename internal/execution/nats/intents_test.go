package nats

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeSubscriber struct {
	subject string
	handler natsbus.Handler
}

func (s *fakeSubscriber) Subscribe(subject string, handler natsbus.Handler) error {
	s.subject, s.handler = subject, handler
	return nil
}

type fakeStore struct{ positions []store.PositionRecord }

func (fakeStore) WithIntentLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}
func (fakeStore) InsertIntent(context.Context, store.OrderIntentRecord) (bool, error) {
	return true, nil
}
func (fakeStore) Intent(_ context.Context, intentID string) (store.OrderIntentRecord, error) {
	return store.OrderIntentRecord{IntentID: intentID, UniqueTag: "lane-a", Kind: store.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy}, nil
}
func (fakeStore) PersistSignedOrder(context.Context, store.SignedOrderRecord) error { return nil }
func (fakeStore) TransitionOrder(_ context.Context, order store.SignedOrderRecord, event statemachine.Event, matched, exchangeID, _ string) (store.SignedOrderRecord, error) {
	transition, _, err := statemachine.Apply(order.State, event)
	if err != nil {
		return store.SignedOrderRecord{}, err
	}
	order.State, order.MatchedShares, order.ExchangeOrderID, order.Revision = transition.To, matched, exchangeID, order.Revision+1
	return order, nil
}
func (fakeStore) OrderByExchangeID(context.Context, string) (store.SignedOrderRecord, error) {
	return store.SignedOrderRecord{}, store.ErrNotFound
}
func (fakeStore) OrderByIntent(context.Context, string, int) (store.SignedOrderRecord, error) {
	return store.SignedOrderRecord{}, store.ErrNotFound
}
func (fakeStore) OpenOrders(context.Context) ([]store.SignedOrderRecord, error) { return nil, nil }
func (fakeStore) Reserve(context.Context, store.ReservationRecord) error        { return nil }
func (fakeStore) Reservation(context.Context, string) (store.ReservationRecord, error) {
	return store.ReservationRecord{}, store.ErrNotFound
}
func (fakeStore) Release(context.Context, string, string) error             { return nil }
func (fakeStore) ApplyFill(context.Context, store.FillRecord) (bool, error) { return false, nil }
func (s fakeStore) PositionFeatures(context.Context) ([]store.PositionRecord, error) {
	return s.positions, nil
}

// fakeCLOB satisfies the executor CLOB interface.
type fakeCLOB struct{}

func (fakeCLOB) CreateOrder(context.Context, clobclient.UserOrder) (clobclient.SignedOrderV2, error) {
	return clobclient.SignedOrderV2{Salt: 1}, nil
}
func (fakeCLOB) SubmitSignedOrder(context.Context, clobclient.SignedOrderV2, clobclient.OrderType, bool) (*clobclient.OrderResponse, error) {
	return &clobclient.OrderResponse{OrderID: "order-1"}, nil
}
func (fakeCLOB) CancelOrder(context.Context, string) error         { return nil }
func (fakeCLOB) TickSize(context.Context, string) (float64, error) { return 0.01, nil }

type recordedPublisher struct {
	subject string
	value   any
}

func (p *recordedPublisher) PublishJSON(subject string, value any) error {
	p.subject = subject
	p.value = value
	return nil
}

func TestSubscribeOpenDecodesAndExecutes(t *testing.T) {
	execution, err := executor.New(fakeStore{}, fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	subscriber := &fakeSubscriber{}
	if err := SubscribeOpen(subscriber, execution); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if subscriber.subject != protocol.SubjectStrategyExecutionOpen || subscriber.handler == nil {
		t.Fatalf("subscription=%+v", subscriber)
	}
	intent := protocol.ExecutionOpenRequest{SchemaVersion: protocol.SchemaVersionV1, UniqueTag: "lane-a", Strategy: "strategy", ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: protocol.SideBuy, TargetUSD: "1", LimitPrice: "0.5", TimeInForce: protocol.TimeInForceGTC, Policy: protocol.ExecutionPolicy{CompleteWithinMillis: 1, Style: protocol.ExecutionStyleLimit}}
	payload, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("deliver: %v", err)
	}
}

func TestSubscribeOpenPublishesFailureForInvalidIntent(t *testing.T) {
	execution, err := executor.New(fakeStore{}, fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	publisher := &recordedPublisher{}
	execution.SetEventPublisher(publisher)
	subscriber := &fakeSubscriber{}
	if err := SubscribeOpen(subscriber, execution); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	intent := protocol.ExecutionOpenRequest{SchemaVersion: protocol.SchemaVersionV1, UniqueTag: "lane-a", Strategy: "strategy", ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: protocol.SideBuy, TargetUSD: "1", LimitPrice: "0.5", TimeInForce: protocol.TimeInForceGTC, Policy: protocol.ExecutionPolicy{CompleteWithinMillis: 1, Style: "UNSUPPORTED"}}
	payload, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err == nil {
		t.Fatal("expected invalid intent error")
	}
	result, ok := publisher.value.(protocol.ExecutionOpenResult)
	if !ok || publisher.subject != protocol.SubjectExecutionOpenResult || result.UniqueTag != "lane-a" || result.ConditionID != "condition" || result.TokenID != "token" || result.Status != protocol.ResultFailed || result.ReasonCode != "UNSUPPORTED_EXECUTION_STYLE" {
		t.Fatalf("result=%+v subject=%q", publisher.value, publisher.subject)
	}
}
