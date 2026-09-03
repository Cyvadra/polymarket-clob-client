package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeStore struct {
	orders  []store.SignedOrderRecord
	updates []update
}

type update struct {
	intentID        string
	exchangeOrderID string
	state           statemachine.State
	event           statemachine.Event
}

func (s *fakeStore) PersistSignedOrder(context.Context, store.SignedOrderRecord) error { return nil }
func (s *fakeStore) OrderByExchangeID(context.Context, string) (store.SignedOrderRecord, error) {
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

type fakeCLOB struct {
	orders     map[string]*clobclient.Order
	openOrders []clobclient.Order
	trades     []clobclient.Trade
	err        error
}

func (c *fakeCLOB) Order(_ context.Context, orderID string) (*clobclient.Order, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.orders[orderID], nil
}

func (c *fakeCLOB) AllOpenOrders(context.Context) ([]clobclient.Order, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.openOrders, nil
}

func (c *fakeCLOB) AllTrades(context.Context) ([]clobclient.Trade, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.trades, nil
}

func TestReconcileMovesUnknownSubmitToLive(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateSubmitUnknown, Revision: 3}}}
	reconciler, err := New(repository, &fakeCLOB{orders: map[string]*clobclient.Order{"order-1": {ID: "order-1", Status: "LIVE"}}}, time.Now, time.Second)
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
	reconciler, _ := New(repository, &fakeCLOB{orders: map[string]*clobclient.Order{"order-1": {ID: "order-1", Status: "CANCELED"}}}, time.Now, time.Second)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateCanceled {
		t.Fatalf("expected canceled observation, got %#v", repository.updates)
	}
}

func TestReconcilePersistsMatchedSharesFromRepeatedLiveObservation(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 3, MatchedShares: "1"}}}
	reconciler, _ := New(repository, &fakeCLOB{orders: map[string]*clobclient.Order{"order-1": {ID: "order-1", Status: "LIVE", SizeMatched: "2"}}}, time.Now, time.Second)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateLive || repository.updates[0].event != statemachine.EventOrderLiveObserved {
		t.Fatalf("expected matched-share observation, got %#v", repository.updates)
	}
}

func TestReconcileWithoutExchangeIDMarksUnknown(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, State: statemachine.StateSubmitUnknown, Revision: 3}}}
	reconciler, _ := New(repository, &fakeCLOB{}, time.Now, time.Second)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateUnknownReconcile {
		t.Fatalf("expected unknown reconcile state, got %#v", repository.updates)
	}
}

func TestReconcileWithoutExchangeIDRecoversUniqueOpenOrder(t *testing.T) {
	signed := clobclient.SignedOrderV2{TokenID: "token", Side: "BUY"}
	payload, err := json.Marshal(signed)
	if err != nil {
		t.Fatalf("marshal signed order: %v", err)
	}
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, SignedPayload: payload, State: statemachine.StateSubmitUnknown, Revision: 3, Price: "0.5", RequestedShares: "2", MatchedShares: "0"}}}
	reconciler, _ := New(repository, &fakeCLOB{openOrders: []clobclient.Order{{ID: "order-1", AssetID: "token", Side: clobclient.SideBuy, Price: "0.500000000", OriginalSize: "2.0", Status: "LIVE", SizeMatched: "0"}}}, time.Now, time.Second)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateLive || repository.updates[0].exchangeOrderID != "order-1" {
		t.Fatalf("expected open-order recovery, got %#v", repository.updates)
	}
}

func TestReconcileWithoutExchangeIDRecoversUniqueTrade(t *testing.T) {
	signed := clobclient.SignedOrderV2{TokenID: "token", Side: "BUY"}
	payload, err := json.Marshal(signed)
	if err != nil {
		t.Fatalf("marshal signed order: %v", err)
	}
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, SignedPayload: payload, State: statemachine.StateSubmitUnknown, Revision: 3, Price: "0.5", RequestedShares: "2", MatchedShares: "0", CreatedAt: time.Unix(100, 0)}}}
	reconciler, _ := New(repository, &fakeCLOB{trades: []clobclient.Trade{{ID: "trade-1", AssetID: "token", Side: clobclient.SideBuy, Price: "0.500000000", Size: "2.0", Timestamp: "120"}}}, time.Now, time.Second)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateFilled || repository.updates[0].event != statemachine.EventFillObserved {
		t.Fatalf("expected trade recovery to filled, got %#v", repository.updates)
	}
}

func TestReconcileWithoutExchangeIDDoesNotBindAmbiguousTrade(t *testing.T) {
	signed := clobclient.SignedOrderV2{TokenID: "token", Side: "BUY"}
	payload, err := json.Marshal(signed)
	if err != nil {
		t.Fatalf("marshal signed order: %v", err)
	}
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, SignedPayload: payload, State: statemachine.StateSubmitUnknown, Revision: 3, Price: "0.5", RequestedShares: "2", MatchedShares: "0"}}}
	reconciler, _ := New(repository, &fakeCLOB{trades: []clobclient.Trade{
		{ID: "trade-1", AssetID: "token", Side: clobclient.SideBuy, Price: "0.5", Size: "2"},
		{ID: "trade-2", AssetID: "token", Side: clobclient.SideBuy, Price: "0.5", Size: "2"},
	}}, time.Now, time.Second)
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(repository.updates) != 1 || repository.updates[0].state != statemachine.StateUnknownReconcile {
		t.Fatalf("expected ambiguous trade to remain unknown, got %#v", repository.updates)
	}
}

func TestReconcileDoesNotChangeStateOnLookupFailure(t *testing.T) {
	repository := &fakeStore{orders: []store.SignedOrderRecord{{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 3}}}
	reconciler, _ := New(repository, &fakeCLOB{err: errors.New("network failure")}, time.Now, time.Second)
	if err := reconciler.Reconcile(context.Background()); err == nil {
		t.Fatal("expected lookup error")
	}
	if len(repository.updates) != 0 {
		t.Fatalf("expected no state change, got %#v", repository.updates)
	}
}
