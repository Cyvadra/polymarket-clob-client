package reconciler

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/pkg/accountfeed"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeStore struct {
	orders  []store.SignedOrderRecord
	updates []update
	fills   []store.FillRecord
}

type update struct {
	intentID        string
	exchangeOrderID string
	state           statemachine.State
	event           statemachine.Event
}

func (s *fakeStore) PersistSignedOrder(context.Context, store.SignedOrderRecord) error { return nil }
func (s *fakeStore) OrderByExchangeID(_ context.Context, orderID string) (store.SignedOrderRecord, error) {
	for _, order := range s.orders {
		if order.ExchangeOrderID == orderID {
			return order, nil
		}
	}
	return store.SignedOrderRecord{}, store.ErrNotFound
}
func (s *fakeStore) OrderByIntent(context.Context, string, int) (store.SignedOrderRecord, error) {
	return store.SignedOrderRecord{}, store.ErrNotFound
}
func (s *fakeStore) OpenOrders(context.Context) ([]store.SignedOrderRecord, error) {
	return s.orders, nil
}
func (s *fakeStore) TransitionOrder(_ context.Context, order store.SignedOrderRecord, event statemachine.Event, matchedShares, exchangeOrderID, _ string) (store.SignedOrderRecord, error) {
	transition, _, err := statemachine.Apply(order.State, event)
	if err != nil {
		return store.SignedOrderRecord{}, err
	}
	s.updates = append(s.updates, update{intentID: order.IntentID, exchangeOrderID: exchangeOrderID, state: transition.To, event: event})
	order.State, order.Revision, order.MatchedShares = transition.To, order.Revision+1, matchedShares
	if exchangeOrderID != "" {
		order.ExchangeOrderID = exchangeOrderID
	}
	return order, nil
}
func (s *fakeStore) WithIntentLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}
func (s *fakeStore) InsertIntent(context.Context, store.OrderIntentRecord) (bool, error) {
	return false, nil
}
func (s *fakeStore) Intent(context.Context, string) (store.OrderIntentRecord, error) {
	return store.OrderIntentRecord{}, store.ErrNotFound
}
func (s *fakeStore) ApplyFill(_ context.Context, record store.FillRecord) (bool, error) {
	s.fills = append(s.fills, record)
	return true, nil
}
func (s *fakeStore) PositionFeatures(context.Context) ([]store.PositionRecord, error) {
	return nil, nil
}
func (s *fakeStore) Reserve(context.Context, store.ReservationRecord) error { return nil }
func (s *fakeStore) Reservation(context.Context, string) (store.ReservationRecord, error) {
	return store.ReservationRecord{}, store.ErrNotFound
}
func (s *fakeStore) Release(context.Context, string, string) error { return nil }

type fakeCLOB struct {
	orders map[string]*clobclient.Order
	trades []clobclient.Trade
	err    error
}

func (c *fakeCLOB) Order(_ context.Context, orderID string) (*clobclient.Order, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.orders[orderID], nil
}

func (c *fakeCLOB) AllTrades(context.Context) ([]clobclient.Trade, error) {
	return c.trades, nil
}

func TestReconcileMovesUnknownSubmitToLive(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateSubmitUnknown, Revision: 3}}}
	reconciler, err := New(repository, &fakeCLOB{orders: map[string]*clobclient.Order{"order-1": {ID: "order-1", Status: "LIVE"}}}, nil, "", time.Now, time.Second)
	if err != nil {
		t.Fatalf("new reconciler: %v", err)
	}
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateLive || repository.updates[0].event != statemachine.EventOrderLiveObserved {
		t.Fatalf("expected live observation, got %#v", repository.updates)
	}
}

func TestReconcileMovesCancelPendingToCanceled(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateCancelPending, Revision: 3}}}
	reconciler, _ := New(repository, &fakeCLOB{orders: map[string]*clobclient.Order{"order-1": {ID: "order-1", Status: "CANCELED"}}}, nil, "", time.Now, time.Second)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateCanceled {
		t.Fatalf("expected canceled observation, got %#v", repository.updates)
	}
}

func TestReconcilePersistsMatchedSharesFromRepeatedLiveObservation(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 3, MatchedShares: "1"}}}
	reconciler, _ := New(repository, &fakeCLOB{orders: map[string]*clobclient.Order{"order-1": {ID: "order-1", Status: "LIVE", SizeMatched: "2"}}}, nil, "", time.Now, time.Second)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].state != statemachine.StatePartiallyFilled || repository.updates[0].event != statemachine.EventPartialFillObserved {
		t.Fatalf("expected matched-share observation, got %#v", repository.updates)
	}
}

func TestReconcileWithoutExchangeIDMarksUnknown(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, State: statemachine.StateSubmitUnknown, Revision: 3}}}
	reconciler, _ := New(repository, &fakeCLOB{}, nil, "", time.Now, time.Second)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateUnknownReconcile {
		t.Fatalf("expected unknown reconcile state, got %#v", repository.updates)
	}
}

func TestReconcileUsesPersistedSignedOrderID(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-hash", State: statemachine.StateSubmitUnknown, Revision: 3, Price: "0.5", RequestedShares: "2", MatchedShares: "0"}}}
	reconciler, _ := New(repository, &fakeCLOB{orders: map[string]*clobclient.Order{"order-hash": {ID: "order-hash", AssetID: "token", Side: clobclient.SideBuy, Price: "0.5", OriginalSize: "2", Status: "LIVE", SizeMatched: "0"}}}, nil, "", time.Now, time.Second)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].exchangeOrderID != "order-hash" || repository.updates[0].state != statemachine.StateLive {
		t.Fatalf("expected order-hash recovery, got %#v", repository.updates)
	}
}

func TestReconcileMarksMissingUnknownSubmissionInconclusive(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateSubmitUnknown, Revision: 3, UpdatedAt: time.Unix(100, 0).UTC()}}}
	reconciler, _ := New(repository, &fakeCLOB{err: &clobclient.APIError{StatusCode: http.StatusNotFound}}, nil, "", func() time.Time { return time.Unix(101, 0).UTC() }, time.Second)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateUnknownReconcile || repository.updates[0].event != statemachine.EventReconcileInconclusive {
		t.Fatalf("expected inconclusive missing submit, got %#v", repository.updates)
	}
}

func TestReconcileFailsMissingUnknownSubmissionAfterGrace(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateUnknownReconcile, Revision: 4, UpdatedAt: time.Unix(100, 0).UTC()}}}
	reconciler, _ := New(repository, &fakeCLOB{err: &clobclient.APIError{StatusCode: http.StatusNotFound}}, nil, "", func() time.Time { return time.Unix(200, 0).UTC() }, time.Second)
	reconciler.SetMissingOrderGrace(time.Minute)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateFailed || repository.updates[0].event != statemachine.EventFailedObserved {
		t.Fatalf("expected failed missing submit after grace, got %#v", repository.updates)
	}
}

func TestReconcileDoesNotChangeStateOnLookupFailure(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 3}}}
	reconciler, _ := New(repository, &fakeCLOB{err: errors.New("network failure")}, nil, "", time.Now, time.Second)
	if err := reconciler.Reconcile(context.Background()); err == nil {
		t.Fatal("expected lookup error")
	}
	if len(repository.updates) != 0 {
		t.Fatalf("expected no state change, got %#v", repository.updates)
	}
}

func TestReconcileReplaysOwnedTradesBeforeOrders(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 3}}}
	fills, err := accountfeed.NewFillConsumer(repository, func() time.Time { return time.Unix(20, 0).UTC() })
	if err != nil {
		t.Fatalf("new fill consumer: %v", err)
	}
	client := &fakeCLOB{
		orders: map[string]*clobclient.Order{"order-1": {ID: "order-1", Status: "LIVE"}},
		trades: []clobclient.Trade{{ID: "trade-1", TakerOrderID: "other-taker", Market: "condition", AssetID: "token", Side: clobclient.SideBuy, Size: "2", Price: "0.5", Outcome: "Up", Status: "CONFIRMED", TraderSide: "MAKER", MakerOrders: []clobclient.MakerTrade{{OrderID: "order-1", Owner: "key", MatchedAmount: "2", Price: "0.5", AssetID: "token", Outcome: "Up", Side: clobclient.SideSell}, {OrderID: "other-maker", Owner: "someone", MatchedAmount: "2", Price: "0.5", AssetID: "token", Outcome: "Up", Side: clobclient.SideSell}}}},
	}
	reconciler, _ := New(repository, client, fills, "key", time.Now, time.Second)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.fills) != 1 || repository.fills[0].ExchangeOrderID != "order-1" || repository.fills[0].IntentID != "intent-1" || repository.fills[0].Side != store.SideSell {
		t.Fatalf("expected owned maker fill replay, got %+v", repository.fills)
	}
}

func TestReconcileMarksSubmittingMissingOrderInconclusive(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateSubmitting, Revision: 2, UpdatedAt: time.Unix(100, 0).UTC()}}}
	reconciler, _ := New(repository, &fakeCLOB{err: &clobclient.APIError{StatusCode: http.StatusNotFound}}, nil, "", func() time.Time { return time.Unix(101, 0).UTC() }, time.Second)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateUnknownReconcile || repository.updates[0].event != statemachine.EventReconcileInconclusive {
		t.Fatalf("expected submitting order to become inconclusive, got %#v", repository.updates)
	}
}

func TestReconcileFailsSubmittingMissingOrderAfterGrace(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateSubmitting, Revision: 2, UpdatedAt: time.Unix(100, 0).UTC()}}}
	reconciler, _ := New(repository, &fakeCLOB{err: &clobclient.APIError{StatusCode: http.StatusNotFound}}, nil, "", func() time.Time { return time.Unix(200, 0).UTC() }, time.Second)
	reconciler.SetMissingOrderGrace(time.Minute)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateFailed || repository.updates[0].event != statemachine.EventFailedObserved {
		t.Fatalf("expected submitting order to fail after grace, got %#v", repository.updates)
	}
}
