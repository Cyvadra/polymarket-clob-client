package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeStore struct {
	inserted   bool
	intentSeen bool
	order      store.SignedOrderRecord
	intent     store.OrderIntentRecord
	positions  []store.PositionRecord
}

func (s *fakeStore) WithIntentLock(ctx context.Context, _ string, fn func(context.Context) error) error {
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
func (s *fakeStore) OrderByIntent(_ context.Context, intentID string, childSequence int) (store.SignedOrderRecord, error) {
	if s.order.IntentID != intentID || s.order.ChildSequence != childSequence {
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
func (s *fakeStore) Reserve(context.Context, store.ReservationRecord) error { return nil }
func (s *fakeStore) Reservation(context.Context, string) (store.ReservationRecord, error) {
	return store.ReservationRecord{}, store.ErrNotFound
}
func (s *fakeStore) Release(context.Context, string, string) error             { return nil }
func (s *fakeStore) ApplyFill(context.Context, store.FillRecord) (bool, error) { return false, nil }
func (s *fakeStore) PositionFeatures(context.Context) ([]store.PositionRecord, error) {
	return s.positions, nil
}

type fakeCLOB struct {
	submitErr       error
	response        *clobclient.OrderResponse
	created         clobclient.UserOrder
	canceledOrderID string
}

func (c *fakeCLOB) TickSize(context.Context, string) (float64, error) { return 0.01, nil }
func (c *fakeCLOB) CreateOrder(_ context.Context, order clobclient.UserOrder) (clobclient.SignedOrderV2, error) {
	c.created = order
	return clobclient.SignedOrderV2{OrderID: "order-1", Salt: 1}, nil
}
func (c *fakeCLOB) SubmitSignedOrder(_ context.Context, _ clobclient.SignedOrderV2, _ clobclient.OrderType, _ bool) (*clobclient.OrderResponse, error) {
	return c.response, c.submitErr
}
func (c *fakeCLOB) CancelOrder(_ context.Context, orderID string) error {
	c.canceledOrderID = orderID
	return nil
}

type resultPublisher struct {
	subject string
	value   any
}

func (p *resultPublisher) PublishJSON(subject string, value any) error {
	p.subject, p.value = subject, value
	return nil
}

func openRequest() protocol.ExecutionOpenRequest {
	return protocol.ExecutionOpenRequest{SchemaVersion: protocol.SchemaVersionV1, UniqueTag: "lane-a", Strategy: "strategy", ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: protocol.SideBuy, TargetUSD: "1", LimitPrice: "0.5", TimeInForce: protocol.TimeInForceGTC, ExpiresAt: time.Now().Add(time.Hour).UTC(), Policy: protocol.ExecutionPolicy{Style: protocol.ExecutionStyleLimit, CompleteWithinMillis: 1}}
}

func TestExecuteOpenPublishesResultOnValidationFailure(t *testing.T) {
	storer := &fakeStore{}
	client := &fakeCLOB{}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	pub := &resultPublisher{}
	exec.SetEventPublisher(pub)
	req := openRequest()
	req.Policy.Style = "UNSUPPORTED"
	if err := exec.ExecuteOpen(context.Background(), req); err == nil {
		t.Fatal("expected validation failure")
	}
	if pub.subject != protocol.SubjectExecutionOpenResult {
		t.Fatalf("expected open result subject, got %q", pub.subject)
	}
	if result, ok := pub.value.(protocol.ExecutionOpenResult); !ok || result.Status != protocol.ResultFailed {
		t.Fatalf("unexpected result: %+v", pub.value)
	}
}

func TestExecuteOpenPersistsAndSubmits(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := exec.ExecuteOpen(context.Background(), openRequest()); err != nil {
		t.Fatalf("execute open: %v", err)
	}
	if client.created.Price != 0.5 || storer.order.ExchangeOrderID != "order-1" {
		t.Fatalf("unexpected execution: created=%+v order=%+v", client.created, storer.order)
	}
}

func TestExecuteOpenMarksUnknownWhenSubmissionTimesOut(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{submitErr: errors.New("timeout")}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := exec.ExecuteOpen(context.Background(), openRequest()); err == nil {
		t.Fatal("expected unknown outcome error")
	}
	if storer.order.State != statemachine.StateSubmitUnknown {
		t.Fatalf("expected submit unknown state, got %s", storer.order.State)
	}
}

type recordingPublisher struct {
	publishes []publishRecord
}

type publishRecord struct {
	subject string
	value   any
}

func (p *recordingPublisher) PublishJSON(subject string, value any) error {
	p.publishes = append(p.publishes, publishRecord{subject: subject, value: value})
	return nil
}

func TestExecuteCloseValidationFailurePublishesOnlyCloseResult(t *testing.T) {
	storer := &fakeStore{inserted: true, positions: []store.PositionRecord{{ConditionID: "condition", TokenID: "token", Outcome: "Up", PositionSize: "2", ActualShares: "2", AvailableSize: "2", State: "open"}}}
	client := &fakeCLOB{}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	pub := &recordingPublisher{}
	exec.SetEventPublisher(pub)
	req := closeRequest(protocol.ExecutionCloseModeLimit)
	req.TimeInForce = protocol.TimeInForce("IOC")
	if err := exec.ExecuteClose(context.Background(), req); err == nil {
		t.Fatal("expected invalid time-in-force failure")
	}
	if len(pub.publishes) != 1 {
		t.Fatalf("expected exactly one result, got %d: %+v", len(pub.publishes), pub.publishes)
	}
	if pub.publishes[0].subject != protocol.SubjectExecutionCloseResult {
		t.Fatalf("expected close result subject, got %q", pub.publishes[0].subject)
	}
	if result, ok := pub.publishes[0].value.(protocol.ExecutionCloseResult); !ok || result.Status != protocol.ResultFailed {
		t.Fatalf("expected failed close result, got %+v", pub.publishes[0].value)
	}
}
