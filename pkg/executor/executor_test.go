package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeStore struct {
	inserted            bool
	intentSeen          bool
	order               store.SignedOrderRecord
	intent              store.OrderIntentRecord
	reserveErr          error
	states              []statemachine.State
	revisions           []int64
	reservations        []store.ReservationRecord
	releases            []string
	lockCalls           int
	reservation         store.ReservationRecord
	conflictOnSubmitAck bool
	positions           []store.PositionRecord
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
	if event == statemachine.EventSubmitAcknowledged && s.conflictOnSubmitAck {
		s.order.State = statemachine.StateLive
		s.order.Revision = order.Revision + 1
		s.order.MatchedShares = matchedShares
		if exchangeOrderID != "" {
			s.order.ExchangeOrderID = exchangeOrderID
		}
		return store.SignedOrderRecord{}, store.ErrConflict
	}
	transition, _, err := statemachine.Apply(order.State, event)
	if err != nil {
		return store.SignedOrderRecord{}, err
	}
	s.revisions = append(s.revisions, order.Revision)
	s.states = append(s.states, transition.To)
	order.State, order.Revision, order.MatchedShares = transition.To, order.Revision+1, matchedShares
	if s.order.IntentID == order.IntentID && s.order.ChildSequence == order.ChildSequence {
		order.SignedPayload = s.order.SignedPayload
		order.SignedOrderHash = s.order.SignedOrderHash
		order.Salt = s.order.Salt
		order.ExchangeOrderID = s.order.ExchangeOrderID
		order.RequestedShares = s.order.RequestedShares
		order.Price = s.order.Price
		order.OrderType = s.order.OrderType
		order.PostOnly = s.order.PostOnly
	}
	if exchangeOrderID != "" {
		order.ExchangeOrderID = exchangeOrderID
	}
	s.order = order
	return order, nil
}
func (s *fakeStore) OrderByExchangeID(context.Context, string) (store.SignedOrderRecord, error) {
	return store.SignedOrderRecord{}, store.ErrNotFound
}
func (s *fakeStore) OrderByIntent(_ context.Context, intentID string, childSequence int) (store.SignedOrderRecord, error) {
	if s.order.IntentID == "" || s.order.IntentID != intentID || s.order.ChildSequence != childSequence {
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
func (s *fakeStore) ApplyFill(context.Context, store.FillRecord) (bool, error) { return false, nil }
func (s *fakeStore) PositionFeatures(context.Context) ([]store.PositionRecord, error) {
	return s.positions, nil
}

type fakeCLOB struct {
	submitErr         error
	cancelErr         error
	response          *clobclient.OrderResponse
	createdOrderID    string
	submits           int
	cancels           int
	created           clobclient.UserOrder
	submittedType     clobclient.OrderType
	submittedPostOnly bool
}

type failingPublisher struct{}

func (failingPublisher) PublishJSON(string, any) error { return errors.New("nats unavailable") }

type recordedAckPublisher struct {
	acks []protocol.ExecutionIntentAck
}

func (p *recordedAckPublisher) PublishJSON(subject string, value any) error {
	if subject != protocol.SubjectExecutionIntentAck {
		return nil
	}
	ack, ok := value.(protocol.ExecutionIntentAck)
	if ok {
		p.acks = append(p.acks, ack)
	}
	return nil
}

func (c *fakeCLOB) CreateOrder(_ context.Context, order clobclient.UserOrder) (clobclient.SignedOrderV2, error) {
	c.created = order
	orderID := c.createdOrderID
	if orderID == "" {
		orderID = "order-1"
	}
	return clobclient.SignedOrderV2{OrderID: orderID, Salt: 42, TokenID: "token", Signature: "signature"}, nil
}
func (c *fakeCLOB) SubmitSignedOrder(_ context.Context, _ clobclient.SignedOrderV2, orderType clobclient.OrderType, postOnly bool) (*clobclient.OrderResponse, error) {
	c.submits++
	c.submittedType = orderType
	c.submittedPostOnly = postOnly
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
	if storer.order.ExchangeOrderID != "order-1" {
		t.Fatalf("expected signed order ID to be persisted, got %q", storer.order.ExchangeOrderID)
	}
	if storer.lockCalls != 1 {
		t.Fatalf("expected intent lock, got %d calls", storer.lockCalls)
	}
	if len(storer.releases) != 0 {
		t.Fatalf("reservation released after submit side effect: %v", storer.releases)
	}
}

func TestExecuteSubmitsWhenTransitionPublicationFails(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	executor.SetEventPublisher(failingPublisher{})
	if err := executor.Execute(context.Background(), testIntent()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if client.submits != 1 || storer.order.State != statemachine.StateLive {
		t.Fatalf("submission must proceed despite publication failure: submits=%d order=%+v", client.submits, storer.order)
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

func TestExecuteMarksUnknownWhenSubmittedOrderIDDiffers(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{createdOrderID: "signed-order", response: &clobclient.OrderResponse{Success: true, OrderID: "different-order"}}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := executor.Execute(context.Background(), testIntent()); err == nil {
		t.Fatal("expected mismatched order ID to be unknown")
	}
	if client.submits != 1 || len(storer.states) != 2 || storer.states[1] != statemachine.StateSubmitUnknown {
		t.Fatalf("expected unknown state after mismatched order ID: submits=%d states=%v", client.submits, storer.states)
	}
	if storer.order.ExchangeOrderID != "signed-order" {
		t.Fatalf("expected persisted signed order ID to be retained, got %q", storer.order.ExchangeOrderID)
	}
}

func TestExecuteMarksRejectedResponseAsRejected(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{submitErr: &clobclient.OrderRejectedError{Message: "insufficient balance"}}
	publisher := &recordedAckPublisher{}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	executor.SetEventPublisher(publisher)
	if err := executor.Execute(context.Background(), testIntent()); err == nil {
		t.Fatal("expected rejected order error")
	}
	if client.submits != 1 || len(storer.states) != 2 || storer.states[1] != statemachine.StateRejected {
		t.Fatalf("expected one submit then rejected state: submits=%d states=%v", client.submits, storer.states)
	}
	if len(publisher.acks) != 1 || publisher.acks[0].Status != protocol.IntentRejected || publisher.acks[0].ReasonCode != "ORDER_REJECTED" || publisher.acks[0].Reason != "insufficient balance" {
		t.Fatalf("unexpected rejection ack: %+v", publisher.acks)
	}
}

func TestExecuteMarksAPIErrorAsUnknown(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{submitErr: &clobclient.APIError{StatusCode: http.StatusUnauthorized}}
	publisher := &recordedAckPublisher{}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	executor.SetEventPublisher(publisher)
	if err := executor.Execute(context.Background(), testIntent()); err == nil {
		t.Fatal("expected API error")
	}
	if client.submits != 1 || len(storer.states) != 2 || storer.states[1] != statemachine.StateSubmitUnknown {
		t.Fatalf("expected API error to be unknown, submits=%d states=%v", client.submits, storer.states)
	}
	if len(publisher.acks) != 1 || publisher.acks[0].Status != protocol.IntentFailed || publisher.acks[0].ReasonCode != "EXECUTION_FAILED" {
		t.Fatalf("unexpected API error ack: %+v", publisher.acks)
	}
}

func TestExecuteAcceptsConcurrentSubmitObservation(t *testing.T) {
	storer := &fakeStore{inserted: true, conflictOnSubmitAck: true}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	publisher := &recordedAckPublisher{}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	executor.SetEventPublisher(publisher)
	if err := executor.Execute(context.Background(), testIntent()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if client.submits != 1 || storer.order.State != statemachine.StateLive {
		t.Fatalf("expected concurrently observed live order, submits=%d order=%+v", client.submits, storer.order)
	}
	if len(publisher.acks) != 1 || publisher.acks[0].Status != protocol.IntentAccepted {
		t.Fatalf("expected accepted ack, got %+v", publisher.acks)
	}
}

func TestExecutePublishesSanitizedReserveRejection(t *testing.T) {
	storer := &fakeStore{inserted: true, reserveErr: store.ErrConflict}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	publisher := &recordedAckPublisher{}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	executor.SetEventPublisher(publisher)
	intent := testIntent()
	intent.Kind = protocol.IntentClose
	intent.Side = protocol.SideSell
	if err := executor.Execute(context.Background(), intent); err == nil {
		t.Fatal("expected reserve rejection")
	}
	if client.submits != 0 || len(publisher.acks) != 1 || publisher.acks[0].Status != protocol.IntentRejected || publisher.acks[0].ReasonCode != "NO_POSITION" || publisher.acks[0].Reason != "no available position for close intent" {
		t.Fatalf("unexpected reserve rejection: submits=%d acks=%+v", client.submits, publisher.acks)
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
	storer.intent = store.OrderIntentRecord{IntentID: "intent-1", CreatedAt: now.Add(-time.Second), Policy: protocol.ExecutionPolicy{CompleteWithinMillis: 1}}
	if err := executor.cancelExpired(context.Background()); err != nil {
		t.Fatalf("cancel elapsed order: %v", err)
	}
	if client.cancels != 1 || len(storer.states) != 2 || storer.states[0] != statemachine.StateCancelRequested || storer.states[1] != statemachine.StateCancelPending {
		t.Fatalf("unexpected cancellation state: cancels=%d states=%v", client.cancels, storer.states)
	}
}

func TestExecuteSkipsDuplicateSubmittedIntent(t *testing.T) {
	storer := &fakeStore{intent: intentRecord(testIntent(), time.Unix(1, 0).UTC()), order: store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, State: statemachine.StateSubmitUnknown, Revision: 2}}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	publisher := &recordedAckPublisher{}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	executor.SetEventPublisher(publisher)
	if err := executor.Execute(context.Background(), testIntent()); err != nil {
		t.Fatalf("execute duplicate: %v", err)
	}
	if client.submits != 0 {
		t.Fatalf("duplicate unresolved intent submitted %d orders", client.submits)
	}
	if len(publisher.acks) != 1 || publisher.acks[0].Status != protocol.IntentAccepted {
		t.Fatalf("expected duplicate accepted ack, got %+v", publisher.acks)
	}
}

func TestExecuteRejectsDuplicateIntentWithDifferentPayload(t *testing.T) {
	stored := testIntent()
	incoming := testIntent()
	incoming.LimitPrice = "0.55"
	storer := &fakeStore{intent: intentRecord(stored, time.Unix(1, 0).UTC()), order: store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, State: statemachine.StateSubmitUnknown, Revision: 2}}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	publisher := &recordedAckPublisher{}
	executor, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	executor.SetEventPublisher(publisher)
	if err := executor.Execute(context.Background(), incoming); err == nil {
		t.Fatal("expected duplicate payload rejection")
	}
	if client.submits != 0 {
		t.Fatalf("duplicate mismatched intent submitted %d orders", client.submits)
	}
	if len(publisher.acks) != 1 || publisher.acks[0].Status != protocol.IntentRejected || publisher.acks[0].ReasonCode != "DUPLICATE_INTENT" || publisher.acks[0].Reason != "intent ID already belongs to a different intent" {
		t.Fatalf("unexpected duplicate rejection ack: %+v", publisher.acks)
	}
}

func TestExecuteResumesPersistedSignedOrder(t *testing.T) {
	signed := clobclient.SignedOrderV2{Salt: 42, TokenID: "token", Signature: "signature"}
	payload, err := json.Marshal(signed)
	if err != nil {
		t.Fatalf("marshal signed order: %v", err)
	}
	storer := &fakeStore{intent: intentRecord(testIntent(), time.Unix(1, 0).UTC()), order: store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, SignedPayload: payload, State: statemachine.StateSigned, Revision: 5}, reservation: store.ReservationRecord{ReservationID: "intent-1:1", State: "active"}}
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
	storer := &fakeStore{intent: intentRecord(testIntent(), time.Unix(1, 0).UTC()), reservation: store.ReservationRecord{ReservationID: "intent-1:1", IntentID: "intent-1", State: "active"}}
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

func TestValidateIntentAcceptsMakerPostOnlyBuyWithinMaxPrice(t *testing.T) {
	intent := testIntent()
	intent.PostOnly = true
	intent.Policy.Style = protocol.ExecutionStyleMakerPostOnly
	intent.Policy.InitialPrice = "0.50"
	intent.Policy.MaxPrice = "0.55"
	intent.Policy.PriceStep = "0.01"
	intent.Policy.QuoteMaxAgeMillis = 500
	if err := validateIntentAt(intent, time.Unix(10, 0).UTC()); err != nil {
		t.Fatalf("validate maker policy: %v", err)
	}
}

func TestValidateIntentRejectsUnimplementedLifecyclePolicy(t *testing.T) {
	intent := testIntent()
	intent.Policy.RepriceIntervalMillis = 250
	if err := validateIntentAt(intent, time.Unix(10, 0).UTC()); err == nil {
		t.Fatal("expected unimplemented lifecycle policy rejection")
	}
}

func TestExecuteUsesPlannerForMakerPostOnlyOrder(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	executor, err := New(storer, client, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	quotes := marketquotes.New()
	if err := quotes.Put(testQuote(now)); err != nil {
		t.Fatalf("put quote: %v", err)
	}
	executor.SetQuoteProvider(quotes)
	intent := testIntent()
	intent.TokenID = "up-token"
	intent.LimitPrice = "0.41"
	intent.Policy.Style = protocol.ExecutionStyleMakerPostOnly
	intent.Policy.InitialPrice = "0.41"
	intent.Policy.MaxPrice = "0.55"
	intent.Policy.PriceStep = "0.01"
	intent.Policy.QuoteOffset = "0.01"
	intent.Policy.QuoteMaxAgeMillis = 500
	if err := executor.Execute(context.Background(), intent); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if client.created.Price != 0.43 || !client.created.PostOnly || client.submittedType != protocol.TimeInForceGTC || !client.submittedPostOnly {
		t.Fatalf("expected planned maker order, created=%+v submittedType=%s submittedPostOnly=%v", client.created, client.submittedType, client.submittedPostOnly)
	}
	if storer.order.Price != "0.43" || !storer.order.PostOnly || storer.reservations[0].Shares != "2" || storer.reservations[0].Notional != "0.860000000000000000" {
		t.Fatalf("expected planned order persistence, order=%+v reservations=%+v", storer.order, storer.reservations)
	}
}

func TestExecuteUsesPlannerForTakerAggressiveOrder(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	executor, err := New(storer, client, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	quotes := marketquotes.New()
	if err := quotes.Put(testQuote(now)); err != nil {
		t.Fatalf("put quote: %v", err)
	}
	executor.SetQuoteProvider(quotes)
	intent := testIntent()
	intent.TokenID = "up-token"
	intent.LimitPrice = "0.41"
	intent.TimeInForce = protocol.TimeInForceGTC
	intent.Policy.Style = protocol.ExecutionStyleTakerAggressive
	intent.Policy.InitialPrice = "0.41"
	intent.Policy.MaxPrice = "0.55"
	intent.Policy.PriceStep = "0.01"
	intent.Policy.QuoteMaxAgeMillis = 500
	if err := executor.Execute(context.Background(), intent); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if client.created.Price != 0.47 || client.created.OrderType != protocol.TimeInForceFAK || client.submittedType != protocol.TimeInForceFAK {
		t.Fatalf("expected planned taker order, created=%+v submittedType=%s", client.created, client.submittedType)
	}
}

func TestValidateIntentRejectsBuyPolicyAboveMaxPrice(t *testing.T) {
	intent := testIntent()
	intent.Policy.Style = protocol.ExecutionStyleTakerAggressive
	intent.Policy.InitialPrice = "0.60"
	intent.Policy.MaxPrice = "0.55"
	if err := validateIntentAt(intent, time.Unix(10, 0).UTC()); err == nil {
		t.Fatal("expected invalid buy policy")
	}
}

func TestValidateIntentRejectsSellPolicyBelowMinPrice(t *testing.T) {
	intent := testIntent()
	intent.Side = protocol.SideSell
	intent.Policy.Style = protocol.ExecutionStyleMakerPostOnly
	intent.Policy.InitialPrice = "0.40"
	intent.Policy.MinPrice = "0.45"
	if err := validateIntentAt(intent, time.Unix(10, 0).UTC()); err == nil {
		t.Fatal("expected invalid sell policy")
	}
}

func testIntent() protocol.ExecutionIntent {
	return protocol.ExecutionIntent{SchemaVersion: protocol.SchemaVersionV1, IntentID: "intent-1", IdempotencyKey: "key-1", Strategy: "strategy", Kind: protocol.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: protocol.SideBuy, TargetShares: "2", LimitPrice: "0.5", TimeInForce: protocol.TimeInForceGTC, Policy: protocol.ExecutionPolicy{CompleteWithinMillis: 60_000}}
}

func testQuote(at time.Time) marketquotes.Snapshot {
	return marketquotes.Snapshot{ConditionID: "condition", At: at, Up: marketquotes.Quote{AssetID: "up-token", Bid: 0.42, Ask: 0.46, Mid: 0.44, Timestamp: at}, Down: marketquotes.Quote{AssetID: "down-token", Bid: 0.52, Ask: 0.56, Mid: 0.54, Timestamp: at}}
}
