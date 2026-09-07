package executor

import (
	"context"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type recordedCancelPublisher struct {
	cancelAcks []protocol.ExecutionCancelAck
	intentAcks []protocol.ExecutionIntentAck
}

func (p *recordedCancelPublisher) PublishJSON(subject string, value any) error {
	switch subject {
	case protocol.SubjectExecutionCancelAck:
		if ack, ok := value.(protocol.ExecutionCancelAck); ok {
			p.cancelAcks = append(p.cancelAcks, ack)
		}
	case protocol.SubjectExecutionIntentAck:
		if ack, ok := value.(protocol.ExecutionIntentAck); ok {
			p.intentAcks = append(p.intentAcks, ack)
		}
	}
	return nil
}

func cancelRequest(intentID string) protocol.ExecutionCancelRequest {
	return protocol.ExecutionCancelRequest{SchemaVersion: protocol.SchemaVersionV1, IntentID: intentID}
}

func forceCancelRequest(intentID string) protocol.ExecutionCancelRequest {
	return protocol.ExecutionCancelRequest{SchemaVersion: protocol.SchemaVersionV1, IntentID: intentID, Force: true}
}

func openPosition() store.PositionRecord {
	return store.PositionRecord{MarketID: "market", ConditionID: "condition", TokenID: "token", Outcome: "Up", PositionSize: "2", AvailableSize: "2", ReservedSize: "0", State: "open"}
}

func TestCancelRejectsMissingSchemaVersion(t *testing.T) {
	storer := &fakeStore{}
	client := &fakeCLOB{}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	publisher := &recordedCancelPublisher{}
	executor.SetEventPublisher(publisher)
	if err := executor.Cancel(context.Background(), protocol.ExecutionCancelRequest{IntentID: "intent-1"}); err == nil {
		t.Fatal("expected schema version rejection")
	}
	if len(publisher.cancelAcks) != 1 || publisher.cancelAcks[0].Status != protocol.CancelFailed || publisher.cancelAcks[0].ReasonCode != "INVALID_CANCEL" {
		t.Fatalf("unexpected invalid cancel ack: %+v", publisher.cancelAcks)
	}
}

func TestCancelAcknowledgesUnknownIntent(t *testing.T) {
	storer := &fakeStore{}
	client := &fakeCLOB{}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	publisher := &recordedCancelPublisher{}
	executor.SetEventPublisher(publisher)
	if err := executor.Cancel(context.Background(), cancelRequest("unknown-intent")); err != nil {
		t.Fatalf("cancel unknown intent: %v", err)
	}
	if len(publisher.cancelAcks) != 1 || publisher.cancelAcks[0].Status != protocol.CancelNotFound || publisher.cancelAcks[0].ReasonCode != "INTENT_NOT_FOUND" {
		t.Fatalf("unexpected not found ack: %+v", publisher.cancelAcks)
	}
}

func TestCancelForceClosesOpenedPosition(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	storer := &fakeStore{
		intent: intentRecord(testIntent(), now),
		order:  store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 3, MatchedShares: "2", RequestedShares: "2"},
	}
	storer.positions = []store.PositionRecord{openPosition()}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	executor, err := New(storer, client, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	publisher := &recordedCancelPublisher{}
	executor.SetEventPublisher(publisher)

	if err := executor.Cancel(context.Background(), forceCancelRequest("intent-1")); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if client.cancels != 1 {
		t.Fatalf("expected open entry order to be canceled, got %d", client.cancels)
	}
	if len(storer.reservations) != 1 || storer.reservations[0].ReservationID != "intent-1:2" || storer.reservations[0].Side != store.SideSell {
		t.Fatalf("expected one force close sell reservation, got %+v", storer.reservations)
	}
	if client.created.Side != protocol.SideSell || client.created.OrderType != protocol.TimeInForceFAK || client.created.PostOnly || client.created.Shares != 2 || client.created.Price != 0.01 {
		t.Fatalf("unexpected force close order: %+v", client.created)
	}
	if storer.order.ChildSequence != 2 || storer.order.State != statemachine.StateLive {
		t.Fatalf("expected force close child to be live, order=%+v", storer.order)
	}
	if len(publisher.cancelAcks) != 1 || publisher.cancelAcks[0].Status != protocol.CancelCompleted || publisher.cancelAcks[0].CanceledOrders != 1 {
		t.Fatalf("unexpected cancel ack: %+v", publisher.cancelAcks)
	}
	if len(publisher.intentAcks) != 0 {
		t.Fatalf("force close must not emit an intent ack, got %+v", publisher.intentAcks)
	}
}

func TestCancelWithoutForcePreservesPosition(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	storer := &fakeStore{
		intent: intentRecord(testIntent(), now),
		order:  store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 3, MatchedShares: "2", RequestedShares: "2"},
	}
	storer.positions = []store.PositionRecord{openPosition()}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	executor, err := New(storer, client, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	publisher := &recordedCancelPublisher{}
	executor.SetEventPublisher(publisher)

	if err := executor.Cancel(context.Background(), cancelRequest("intent-1")); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if client.cancels != 1 {
		t.Fatalf("expected open entry order to be canceled, got %d", client.cancels)
	}
	if len(storer.reservations) != 0 {
		t.Fatalf("expected no force close reservation, got %+v", storer.reservations)
	}
	if len(publisher.cancelAcks) != 1 || publisher.cancelAcks[0].Status != protocol.CancelCanceled {
		t.Fatalf("unexpected cancel ack: %+v", publisher.cancelAcks)
	}
	if len(publisher.intentAcks) != 0 {
		t.Fatalf("unexpected intent ack: %+v", publisher.intentAcks)
	}
}

func TestCancelOpenIntentWithoutPositionOnlyCancelsOrder(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	storer := &fakeStore{
		intent: intentRecord(testIntent(), now),
		order:  store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 3},
	}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	executor, err := New(storer, client, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	publisher := &recordedCancelPublisher{}
	executor.SetEventPublisher(publisher)

	if err := executor.Cancel(context.Background(), cancelRequest("intent-1")); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if client.cancels != 1 || len(storer.reservations) != 0 {
		t.Fatalf("expected only the open order to be canceled: cancels=%d reservations=%d", client.cancels, len(storer.reservations))
	}
	if len(publisher.cancelAcks) != 1 || publisher.cancelAcks[0].Status != protocol.CancelCanceled || publisher.cancelAcks[0].CanceledOrders != 1 {
		t.Fatalf("unexpected cancel ack: %+v", publisher.cancelAcks)
	}
}

func TestCancelDoesNotRetryExistingForceClose(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	// A previous cancel already placed child two and it reached a terminal
	// state without filling. A repeated cancel must not place another FAK.
	storer := &fakeStore{
		intent: intentRecord(testIntent(), now),
		order:  store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 2, State: statemachine.StateCanceled, Revision: 6},
	}
	storer.positions = []store.PositionRecord{openPosition()}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	executor, err := New(storer, client, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	publisher := &recordedCancelPublisher{}
	executor.SetEventPublisher(publisher)

	if err := executor.Cancel(context.Background(), forceCancelRequest("intent-1")); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if client.cancels != 0 || client.submits != 0 || len(storer.reservations) != 0 {
		t.Fatalf("repeated cancel must not act again: cancels=%d submits=%d reservations=%d", client.cancels, client.submits, len(storer.reservations))
	}
	if len(publisher.cancelAcks) != 1 || publisher.cancelAcks[0].Status != protocol.CancelCompleted {
		t.Fatalf("unexpected cancel ack: %+v", publisher.cancelAcks)
	}
}

func TestCancelSkipsForceCloseWhenActiveSellReservation(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	storer := &fakeStore{
		intent:     intentRecord(testIntent(), now),
		order:      store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 3},
		reserveErr: store.ErrActiveSellReservation,
	}
	storer.positions = []store.PositionRecord{openPosition()}
	client := &fakeCLOB{}
	executor, err := New(storer, client, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	publisher := &recordedCancelPublisher{}
	executor.SetEventPublisher(publisher)

	if err := executor.Cancel(context.Background(), forceCancelRequest("intent-1")); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if client.submits != 0 {
		t.Fatalf("force close must not be submitted with an active sell reservation, submits=%d", client.submits)
	}
	if len(publisher.cancelAcks) != 1 || publisher.cancelAcks[0].Status != protocol.CancelActiveSellReservation {
		t.Fatalf("unexpected cancel ack: %+v", publisher.cancelAcks)
	}
}

func TestCancelCancelsSignedChildBeforeSubmission(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	storer := &fakeStore{
		intent: intentRecord(testIntent(), now),
		order:  store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, State: statemachine.StateSigned, Revision: 1},
	}
	client := &fakeCLOB{}
	executor, err := New(storer, client, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	publisher := &recordedCancelPublisher{}
	executor.SetEventPublisher(publisher)

	if err := executor.Cancel(context.Background(), cancelRequest("intent-1")); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if client.submits != 0 {
		t.Fatalf("signed child must not be submitted after cancel, submits=%d", client.submits)
	}
	if len(storer.states) != 1 || storer.states[0] != statemachine.StateCanceled {
		t.Fatalf("expected signed child to be canceled, states=%v", storer.states)
	}
	if len(publisher.cancelAcks) != 1 || publisher.cancelAcks[0].Status != protocol.CancelCanceled {
		t.Fatalf("unexpected cancel ack: %+v", publisher.cancelAcks)
	}
}
