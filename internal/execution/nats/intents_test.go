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

type fakeStore struct{}

func (fakeStore) WithIntentLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}
func (fakeStore) InsertIntent(context.Context, store.OrderIntentRecord) (bool, error) {
	return true, nil
}
func (fakeStore) Intent(context.Context, string) (store.OrderIntentRecord, error) {
	return store.OrderIntentRecord{}, store.ErrNotFound
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
func (fakeStore) Release(context.Context, string, string) error                    { return nil }
func (fakeStore) ApplyFill(context.Context, store.FillRecord) (bool, error)        { return false, nil }
func (fakeStore) PositionFeatures(context.Context) ([]store.PositionRecord, error) { return nil, nil }

type fakeCLOB struct{}

func (fakeCLOB) CreateOrder(context.Context, clobclient.UserOrder) (clobclient.SignedOrderV2, error) {
	return clobclient.SignedOrderV2{Salt: 1}, nil
}
func (fakeCLOB) SubmitSignedOrder(context.Context, clobclient.SignedOrderV2, clobclient.OrderType, bool) (*clobclient.OrderResponse, error) {
	return &clobclient.OrderResponse{OrderID: "order-1"}, nil
}
func (fakeCLOB) CancelOrder(context.Context, string) error { return nil }

type recordedPublisher struct {
	subject string
	ack     protocol.ExecutionIntentAck
}

func (p *recordedPublisher) PublishJSON(subject string, value any) error {
	p.subject = subject
	ack, ok := value.(protocol.ExecutionIntentAck)
	if ok {
		p.ack = ack
	}
	return nil
}

func TestSubscribeIntentsDecodesAndExecutes(t *testing.T) {
	execution, err := executor.New(fakeStore{}, fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	subscriber := &fakeSubscriber{}
	if err := SubscribeIntents(subscriber, execution); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if subscriber.subject != protocol.SubjectStrategyExecutionIntent || subscriber.handler == nil {
		t.Fatalf("subscription=%+v", subscriber)
	}
	intent := protocol.ExecutionIntent{SchemaVersion: protocol.SchemaVersionV1, IntentID: "intent", IdempotencyKey: "key", Strategy: "strategy", Kind: protocol.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: protocol.SideBuy, TargetUSD: "1", LimitPrice: "0.5", TimeInForce: protocol.TimeInForceGTC, Policy: protocol.ExecutionPolicy{CompleteWithinMillis: 1, Style: protocol.ExecutionStyleLimit}}
	payload, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("deliver: %v", err)
	}
}

func TestSubscribeIntentsAcknowledgesDecodableInvalidIntent(t *testing.T) {
	execution, err := executor.New(fakeStore{}, fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	publisher := &recordedPublisher{}
	execution.SetEventPublisher(publisher)
	subscriber := &fakeSubscriber{}
	if err := SubscribeIntents(subscriber, execution); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	intent := protocol.ExecutionIntent{SchemaVersion: protocol.SchemaVersionV1, IntentID: "intent", IdempotencyKey: "key", Strategy: "strategy", Kind: protocol.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: protocol.SideBuy, TargetUSD: "1", LimitPrice: "0.5", TimeInForce: protocol.TimeInForceGTC, Policy: protocol.ExecutionPolicy{CompleteWithinMillis: 1, Style: "UNSUPPORTED"}}
	payload, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err == nil {
		t.Fatal("expected invalid intent error")
	}
	if publisher.subject != protocol.SubjectExecutionIntentAck || publisher.ack.IntentID != intent.IntentID || publisher.ack.Status != protocol.IntentRejected || publisher.ack.ReasonCode != "UNSUPPORTED_EXECUTION_STYLE" {
		t.Fatalf("acknowledgement=%+v subject=%q", publisher.ack, publisher.subject)
	}
}
