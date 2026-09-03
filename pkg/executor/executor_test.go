package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/pkg/contracts"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeStore struct {
	inserted     bool
	intentSeen   bool
	order        store.SignedOrderRecord
	intent       store.OrderIntentRecord
	reserveErr   error
	states       []statemachine.State
	revisions    []int64
	reservations []store.ReservationRecord
	releases     []string
	lockCalls    int
	reservation  store.ReservationRecord
}

func (s *fakeStore) WithIntentLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	s.lockCalls++
	return fn(ctx)
}
func (s *fakeStore) InsertIntent(context.Context, store.OrderIntentRecord) (bool, error) {
	if s.intentSeen {
		return false, nil
	}
	s.intentSeen = true
	return s.inserted, nil
}
func (s *fakeStore) Intent(_ context.Context, intentID string) (store.OrderIntentRecord, error) {
	if s.intent.IntentID != intentID {
		return store.OrderIntentRecord{}, store.ErrNotFound
	}
	return s.intent, nil
}
func (s *fakeStore) PersistSignedOrder(_ context.Context, record store.SignedOrderRecord) error {
	s.order = record
	return nil
}
func (s *fakeStore) TransitionOrder(_ context.Context, order store.SignedOrderRecord, event statemachine.Event, matchedShares, exchangeOrderID, _ string) (store.SignedOrderRecord, error) {
	transition, _, err := statemachine.Apply(order.State, event)
	if err != nil {
		return store.SignedOrderRecord{}, err
	}
	s.revisions = append(s.revisions, order.Revision)
	s.states = append(s.states, transition.To)
	order.State, order.Revision, order.MatchedShares = transition.To, order.Revision+1, matchedShares
	if exchangeOrderID != "" {
		order.ExchangeOrderID = exchangeOrderID
	}
	s.order = order
	return order, nil
}
func (s *fakeStore) OrderByExchangeID(context.Context, string) (store.SignedOrderRecord, error) {
	return store.SignedOrderRecord{}, store.ErrNotFound
}
func (s *fakeStore) OrderByIntent(context.Context, string, int) (store.SignedOrderRecord, error) {
	if s.order.IntentID == "" {
		return store.SignedOrderRecord{}, store.ErrNotFound
	}
	return s.order, nil
}
func (s *fakeStore) OpenOrders(context.Context) ([]store.SignedOrderRecord, error) {
	if s.order.IntentID == "" {
		return nil, nil
	}
	return []store.SignedOrderRecord{s.order}, nil
}
func (s *fakeStore) Reserve(_ context.Context, record store.ReservationRecord) error {
	if s.reserveErr != nil {
		return s.reserveErr
	}
	s.reservations = append(s.reservations, record)
	s.reservation = record
	return nil
}
func (s *fakeStore) Reservation(context.Context, string) (store.ReservationRecord, error) {
	if s.reservation.ReservationID == "" {
		return store.ReservationRecord{}, store.ErrNotFound
	}
	return s.reservation, nil
}
func (s *fakeStore) Release(_ context.Context, reservationID, _ string) error {
	s.releases = append(s.releases, reservationID)
	return nil
}
func (s *fakeStore) ReservationsForPosition(context.Context, string, string) ([]store.ReservationRecord, error) {
	return nil, nil
}

type fakeCLOB struct {
	submitErr error
	cancelErr error
	response  *clobclient.OrderResponse
	submits   int
	cancels   int
}

func (c *fakeCLOB) CreateOrder(context.Context, clobclient.UserOrder) (clobclient.SignedOrderV2, error) {
	return clobclient.SignedOrderV2{Salt: 42, TokenID: "token", Signature: "signature"}, nil
}
func (c *fakeCLOB) SubmitSignedOrder(context.Context, clobclient.SignedOrderV2, clobclient.OrderType, bool) (*clobclient.OrderResponse, error) {
	c.submits++
	return c.response, c.submitErr
}
func (c *fakeCLOB) CancelOrder(context.Context, string) error {
	c.cancels++
	return c.cancelErr
}

func TestExecutePersistsBeforeSubmitting(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	executor, err := New(storer, client, func() time.Time { return time.Unix(1, 0) })
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := executor.Execute(context.Background(), testIntent()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if client.submits != 1 || len(storer.states) != 2 || storer.states[0] != statemachine.StateSubmitting || storer.states[1] != statemachine.StateLive {
		t.Fatalf("unexpected execution sequence: submits=%d states=%v", client.submits, storer.states)
	}
	if len(storer.reservations) != 1 || storer.reservations[0].ReservationID != "intent-1:1" || storer.reservations[0].Notional != "1.000000000000000000" {
		t.Fatalf("expected one active reservation, got %+v", storer.reservations)
	}
	if storer.lockCalls != 1 {
		t.Fatalf("expected intent lock, got %d calls", storer.lockCalls)
	}
	if len(storer.releases) != 0 {
		t.Fatalf("reservation released after submit side effect: %v", storer.releases)
	}
}

func TestExecuteMarksUnknownWithoutRetryingSubmit(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{submitErr: errors.New("timeout")}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := executor.Execute(context.Background(), testIntent()); err == nil {
		t.Fatal("expected unknown outcome error")
	}
	if client.submits != 1 || len(storer.states) != 2 || storer.states[1] != statemachine.StateSubmitUnknown {
		t.Fatalf("expected one submit then unknown state: submits=%d states=%v", client.submits, storer.states)
	}
}

func TestExecuteRejectsExpiredIntentBeforeReserving(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	now := time.Unix(100, 0).UTC()
	executor, err := New(storer, client, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	intent := testIntent()
	intent.ExpiresAt = now.Add(-time.Second)
	if err := executor.Execute(context.Background(), intent); err == nil {
		t.Fatal("expected expired intent rejection")
	}
	if len(storer.reservations) != 0 || client.submits != 0 {
		t.Fatalf("expired intent reached execution: reservations=%d submits=%d", len(storer.reservations), client.submits)
	}
}

func TestCancelExpiredRequestsAndSubmitsCancellation(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	storer := &fakeStore{
		order:  store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 3},
		intent: store.OrderIntentRecord{IntentID: "intent-1"},
	}
	client := &fakeCLOB{}
	executor, err := New(storer, client, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := executor.cancelExpired(context.Background()); err != nil {
		t.Fatalf("cancel expired: %v", err)
	}
	if client.cancels != 0 {
		t.Fatalf("unexpected cancellation without an elapsed deadline: %d", client.cancels)
	}

	storer.order.IntentID = "intent-1"
	storer.order.State = statemachine.StateLive
	storer.order.Revision = 3
	storer.intent = store.OrderIntentRecord{IntentID: "intent-1", CreatedAt: now.Add(-time.Second), Policy: contracts.ExecutionPolicy{CompleteWithinMillis: 1}}
	if err := executor.cancelExpired(context.Background()); err != nil {
		t.Fatalf("cancel elapsed order: %v", err)
	}
	if client.cancels != 1 || len(storer.states) != 2 || storer.states[0] != statemachine.StateCancelRequested || storer.states[1] != statemachine.StateCancelPending {
		t.Fatalf("unexpected cancellation state: cancels=%d states=%v", client.cancels, storer.states)
	}
}

func TestExecuteSkipsDuplicateSubmittedIntent(t *testing.T) {
	storer := &fakeStore{order: store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, State: statemachine.StateSubmitUnknown, Revision: 2}}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := executor.Execute(context.Background(), testIntent()); err != nil {
		t.Fatalf("execute duplicate: %v", err)
	}
	if client.submits != 0 {
		t.Fatalf("duplicate unresolved intent submitted %d orders", client.submits)
	}
}

func TestExecuteResumesPersistedSignedOrder(t *testing.T) {
	signed := clobclient.SignedOrderV2{Salt: 42, TokenID: "token", Signature: "signature"}
	payload, err := json.Marshal(signed)
	if err != nil {
		t.Fatalf("marshal signed order: %v", err)
	}
	storer := &fakeStore{order: store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, SignedPayload: payload, State: statemachine.StateSigned, Revision: 5}, reservation: store.ReservationRecord{ReservationID: "intent-1:1", State: "active"}}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := executor.Execute(context.Background(), testIntent()); err != nil {
		t.Fatalf("execute duplicate signed order: %v", err)
	}
	if client.submits != 1 || len(storer.states) != 2 || storer.states[0] != statemachine.StateSubmitting || storer.states[1] != statemachine.StateLive {
		t.Fatalf("expected signed order resume, submits=%d states=%v", client.submits, storer.states)
	}
	if len(storer.revisions) != 2 || storer.revisions[0] != 5 || storer.revisions[1] != 6 {
		t.Fatalf("expected persisted revisions to be used, got %v", storer.revisions)
	}
}

func TestExecutePreparesOrderWhenDuplicateIntentHasNoOrder(t *testing.T) {
	storer := &fakeStore{reservation: store.ReservationRecord{ReservationID: "intent-1:1", IntentID: "intent-1", State: "active"}}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := executor.Execute(context.Background(), testIntent()); err != nil {
		t.Fatalf("execute duplicate without order: %v", err)
	}
	if client.submits != 1 {
		t.Fatalf("expected duplicate intent without order to submit once, got %d", client.submits)
	}
}

func TestExecuteDoesNotResumeSignedOrderWithoutActiveReservation(t *testing.T) {
	signed := clobclient.SignedOrderV2{Salt: 42, TokenID: "token", Signature: "signature"}
	payload, err := json.Marshal(signed)
	if err != nil {
		t.Fatalf("marshal signed order: %v", err)
	}
	storer := &fakeStore{order: store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, SignedPayload: payload, State: statemachine.StateSigned, Revision: 5}}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := executor.Execute(context.Background(), testIntent()); err == nil {
		t.Fatal("expected inactive reservation to block resume")
	}
	if client.submits != 0 {
		t.Fatalf("signed order without active reservation submitted %d orders", client.submits)
	}
}

func TestExecuteTreatsDuplicateReservationAsIdempotent(t *testing.T) {
	storer := &fakeStore{inserted: true, reserveErr: store.ErrDuplicate, reservation: store.ReservationRecord{ReservationID: "intent-1:1", IntentID: "intent-1", State: "active"}}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := executor.Execute(context.Background(), testIntent()); err != nil {
		t.Fatalf("execute with duplicate reservation: %v", err)
	}
	if client.submits != 1 {
		t.Fatalf("expected duplicate reservation to continue execution, submits=%d", client.submits)
	}
}

func TestExecuteRejectsDuplicateReservationWithoutActiveRecord(t *testing.T) {
	storer := &fakeStore{inserted: true, reserveErr: store.ErrDuplicate}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := executor.Execute(context.Background(), testIntent()); err == nil {
		t.Fatal("expected duplicate reservation without active record to fail")
	}
	if client.submits != 0 {
		t.Fatalf("unexpected submit with unsafe duplicate reservation: %d", client.submits)
	}
}

func testIntent() contracts.ExecutionIntent {
	return contracts.ExecutionIntent{IntentID: "intent-1", IdempotencyKey: "key-1", Strategy: "strategy", Kind: contracts.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: contracts.SideBuy, TargetShares: "2", LimitPrice: "0.5", TimeInForce: contracts.TimeInForceGTC, Policy: contracts.ExecutionPolicy{CompleteWithinMillis: 60_000}}
}
