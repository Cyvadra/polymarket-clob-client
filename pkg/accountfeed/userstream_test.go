package accountfeed

import (
	"context"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type userStreamStore struct {
	order store.SignedOrderRecord
	fills []store.FillRecord
}

func (s *userStreamStore) PersistSignedOrder(context.Context, store.SignedOrderRecord) error {
	return nil
}
func (s *userStreamStore) TransitionOrder(_ context.Context, order store.SignedOrderRecord, event statemachine.Event, matchedShares, exchangeOrderID, _ string) (store.SignedOrderRecord, error) {
	transition, _, err := statemachine.Apply(order.State, event)
	if err != nil {
		return store.SignedOrderRecord{}, err
	}
	s.order.State = transition.To
	s.order.MatchedShares = matchedShares
	s.order.Revision = order.Revision + 1
	if exchangeOrderID != "" {
		s.order.ExchangeOrderID = exchangeOrderID
	}
	return s.order, nil
}
func (s *userStreamStore) OrderByExchangeID(_ context.Context, orderID string) (store.SignedOrderRecord, error) {
	if s.order.ExchangeOrderID != orderID {
		return store.SignedOrderRecord{}, store.ErrNotFound
	}
	return s.order, nil
}
func (s *userStreamStore) OrderByIntent(context.Context, string, int) (store.SignedOrderRecord, error) {
	return store.SignedOrderRecord{}, store.ErrNotFound
}
func (s *userStreamStore) OpenOrders(context.Context) ([]store.SignedOrderRecord, error) {
	return nil, nil
}
func (s *userStreamStore) ApplyFill(_ context.Context, fill store.FillRecord) (bool, error) {
	s.fills = append(s.fills, fill)
	return true, nil
}
func (s *userStreamStore) WithIntentLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}
func (s *userStreamStore) InsertIntent(context.Context, store.OrderIntentRecord) (bool, error) {
	return false, nil
}
func (s *userStreamStore) Intent(_ context.Context, intentID string) (store.OrderIntentRecord, error) {
	if s.order.IntentID != intentID {
		return store.OrderIntentRecord{}, store.ErrNotFound
	}
	return store.OrderIntentRecord{IntentID: intentID, UniqueTag: "lane-a"}, nil
}
func (s *userStreamStore) PositionFeatures(context.Context) ([]store.PositionRecord, error) {
	return nil, nil
}
func (s *userStreamStore) Reserve(context.Context, store.ReservationRecord) error { return nil }
func (s *userStreamStore) Reservation(context.Context, string) (store.ReservationRecord, error) {
	return store.ReservationRecord{}, store.ErrNotFound
}
func (s *userStreamStore) Release(context.Context, string, string) error { return nil }

func TestUserStreamConsumesOrderAndMatchedTrade(t *testing.T) {
	repository := &userStreamStore{order: store.SignedOrderRecord{IntentID: "intent-1", ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 1}}
	orders, err := NewOrderConsumer(repository, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	fills, err := NewFillConsumer(repository, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := NewUserStream(UserStreamConfig{Credentials: clobclient.Credentials{APIKey: "key", Secret: "secret", Passphrase: "pass"}}, orders, fills)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.consume(context.Background(), []byte(`{"event_type":"order","id":"order-1","market":"condition","asset_id":"token","status":"MATCHED","size_matched":"2","timestamp":"1000"}`)); err != nil {
		t.Fatal(err)
	}
	if repository.order.State != statemachine.StateFilled || repository.order.MatchedShares != "2" {
		t.Fatalf("order observation = %+v", repository.order)
	}
	if err := stream.consume(context.Background(), []byte(`{"event_type":"trade","id":"fill-1","taker_order_id":"order-1","market":"condition","asset_id":"token","side":"BUY","size":"2","price":"0.5","outcome":"Up","status":"MATCHED","trader_side":"TAKER","timestamp":"1000"}`)); err != nil {
		t.Fatal(err)
	}
	if len(repository.fills) != 1 || repository.fills[0].FillID != "fill-1" || repository.fills[0].UniqueTag != "lane-a" || !repository.fills[0].ExchangeTime.Equal(time.Unix(1, 0).UTC()) {
		t.Fatalf("fills = %+v", repository.fills)
	}
}

func TestUserStreamOnlyConsumesOwnedMakerTrades(t *testing.T) {
	repository := &userStreamStore{order: store.SignedOrderRecord{IntentID: "intent-1", ExchangeOrderID: "our-maker", State: statemachine.StateLive, Revision: 1}}
	orders, _ := NewOrderConsumer(repository, time.Now)
	fills, _ := NewFillConsumer(repository, time.Now)
	stream, _ := NewUserStream(UserStreamConfig{Credentials: clobclient.Credentials{APIKey: "key", Secret: "secret", Passphrase: "pass"}}, orders, fills)
	if err := stream.consume(context.Background(), []byte(`{"event_type":"trade","id":"fill-1","taker_order_id":"other-taker","market":"condition","asset_id":"token","side":"BUY","size":"2","price":"0.5","outcome":"Up","status":"MATCHED","trader_side":"MAKER","owner":"key","timestamp":"1000","maker_orders":[{"order_id":"our-maker","owner":"key","matched_amount":"1","price":"0.5","asset_id":"token","outcome":"Up","side":"SELL"},{"order_id":"other-maker","owner":"someone-else","matched_amount":"1","price":"0.5","asset_id":"token","outcome":"Up","side":"SELL"}]}`)); err != nil {
		t.Fatal(err)
	}
	if len(repository.fills) != 1 || repository.fills[0].ExchangeOrderID != "our-maker" || repository.fills[0].UniqueTag != "lane-a" || repository.fills[0].TraderSide != "MAKER" {
		t.Fatalf("expected only owned maker fill, got %+v", repository.fills)
	}
}

func TestFillConsumerPersistsUnknownOrder(t *testing.T) {
	repository := &userStreamStore{}
	consumer, err := NewFillConsumer(repository, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := consumer.Consume(context.Background(), AccountFill{
		FillID: "fill-1", ExchangeOrderID: "missing", ConditionID: "condition", TokenID: "token",
		Outcome: "Up", Side: protocol.SideBuy, Shares: "1", Price: "0.5",
	})
	if err != nil || inserted || len(repository.fills) != 0 {
		t.Fatalf("unknown fill must be ignored: inserted=%v fills=%+v err=%v", inserted, repository.fills, err)
	}
}

func TestUserStreamIgnoresUnconfirmedTrade(t *testing.T) {
	repository := &userStreamStore{}
	orders, _ := NewOrderConsumer(repository, time.Now)
	fills, _ := NewFillConsumer(repository, time.Now)
	stream, _ := NewUserStream(UserStreamConfig{Credentials: clobclient.Credentials{APIKey: "key", Secret: "secret", Passphrase: "pass"}}, orders, fills)
	if err := stream.consume(context.Background(), []byte(`{"event_type":"trade","id":"fill-1","status":"MINED"}`)); err != nil {
		t.Fatal(err)
	}
	if len(repository.fills) != 0 {
		t.Fatalf("unconfirmed fill persisted: %+v", repository.fills)
	}
}
