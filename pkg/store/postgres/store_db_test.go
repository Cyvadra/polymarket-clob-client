//go:build postgres

package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// These tests exercise the real SQL invariants (optimistic revision CAS, fill
// reversal, reservation restore, intent status mirroring) against a live
// PostgreSQL database. They are skipped unless EXECUTION_TEST_POSTGRES_URL is
// set, mirroring the opt-in pattern used by the CLOB integration tests.
func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("EXECUTION_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("EXECUTION_TEST_POSTGRES_URL is not set; skipping database-backed store tests")
	}
	ctx := context.Background()
	s, err := New(ctx, Config{URL: url, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func seedIntent(t *testing.T, s *Store, intentID string) {
	t.Helper()
	_, err := s.InsertIntent(context.Background(), store.OrderIntentRecord{
		IntentID: intentID, UniqueTag: "lane-a", Strategy: "test", Kind: store.IntentOpen,
		MarketID: "market", ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy,
		TargetUSD: "1", LimitPrice: "0.5", TimeInForce: store.TimeInForceGTC, Status: statemachine.StateIntentReceived,
	})
	if err != nil {
		t.Fatalf("seed intent: %v", err)
	}
}

func seedSignedOrder(t *testing.T, s *Store, intentID string) {
	t.Helper()
	err := s.PersistSignedOrder(context.Background(), store.SignedOrderRecord{
		IntentID: intentID, ChildSequence: 1, SignedPayload: []byte(`{"salt":1}`), SignedOrderHash: intentID + "-hash", Salt: "1",
		ExchangeOrderID: intentID + "-exchange", RequestedShares: "2", Price: "0.5", OrderType: store.TimeInForceGTC,
		State: statemachine.StateSigned, Revision: 1,
	})
	if err != nil {
		t.Fatalf("seed signed order: %v", err)
	}
}

func TestTransitionOrderRejectsStaleRevision(t *testing.T) {
	s := testStore(t)
	seedIntent(t, s, "intent-cas")
	seedSignedOrder(t, s, "intent-cas")

	first, err := s.TransitionOrder(context.Background(), store.SignedOrderRecord{
		IntentID: "intent-cas", ChildSequence: 1, State: statemachine.StateSigned, Revision: 1, MatchedShares: "0",
	}, statemachine.EventSubmitStarted, "0", "", "submit")
	if err != nil {
		t.Fatalf("first transition: %v", err)
	}
	if first.State != statemachine.StateSubmitting {
		t.Fatalf("expected SUBMITTING, got %s", first.State)
	}

	_, err = s.TransitionOrder(context.Background(), store.SignedOrderRecord{
		IntentID: "intent-cas", ChildSequence: 1, State: statemachine.StateSigned, Revision: 1, MatchedShares: "0",
	}, statemachine.EventSubmitStarted, "0", "", "submit again")
	if err != store.ErrConflict {
		t.Fatalf("expected ErrConflict on stale revision, got %v", err)
	}
}

func TestIntentStatusMirrorsOrderLifecycle(t *testing.T) {
	s := testStore(t)
	seedIntent(t, s, "intent-status")
	seedSignedOrder(t, s, "intent-status")

	_, err := s.TransitionOrder(context.Background(), store.SignedOrderRecord{
		IntentID: "intent-status", ChildSequence: 1, State: statemachine.StateSigned, Revision: 1, MatchedShares: "0",
	}, statemachine.EventSubmitStarted, "0", "", "submit")
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	intent, err := s.Intent(context.Background(), "intent-status")
	if err != nil {
		t.Fatalf("load intent: %v", err)
	}
	if intent.Status != statemachine.StateSubmitting {
		t.Fatalf("expected intent status SUBMITTING, got %s", intent.Status)
	}
}

func TestApplyFillThenFailedReversesPosition(t *testing.T) {
	s := testStore(t)
	seedIntent(t, s, "intent-fill")
	seedSignedOrder(t, s, "intent-fill")

	ctx := context.Background()
	inserted, err := s.ApplyFill(ctx, store.FillRecord{
		FillID: "fill-1", ExchangeOrderID: "intent-fill-exchange", IntentID: "intent-fill", UniqueTag: "lane-a",
		MarketID: "market", ConditionID: "condition", TokenID: "token", Outcome: "Up",
		Side: store.SideBuy, Shares: "10", Price: "0.5", TradeStatus: "CONFIRMED", TraderSide: "TAKER",
	})
	if err != nil || !inserted {
		t.Fatalf("apply fill: inserted=%v err=%v", inserted, err)
	}
	positions, err := s.PositionFeatures(ctx)
	if err != nil {
		t.Fatalf("positions after buy: %v", err)
	}
	if len(positions) != 1 || positions[0].PositionSize != "9.650000000000000000" {
		t.Fatalf("expected taker-buy credited position, got %+v", positions)
	}

	reversed, err := s.ApplyFill(ctx, store.FillRecord{
		FillID: "fill-1", ExchangeOrderID: "intent-fill-exchange", IntentID: "intent-fill", UniqueTag: "lane-a",
		MarketID: "market", ConditionID: "condition", TokenID: "token", Outcome: "Up",
		Side: store.SideBuy, Shares: "10", Price: "0.5", TradeStatus: "FAILED", TraderSide: "TAKER",
	})
	if err != nil || !reversed {
		t.Fatalf("reverse fill: reversed=%v err=%v", reversed, err)
	}
	positions, err = s.PositionFeatures(ctx)
	if err != nil {
		t.Fatalf("positions after reversal: %v", err)
	}
	if len(positions) != 1 || positions[0].PositionSize != "0" || positions[0].AvailableSize != "0" {
		t.Fatalf("expected fully reversed position, got %+v", positions)
	}
}

func TestReserveReleaseRestoresAvailableShares(t *testing.T) {
	s := testStore(t)
	seedIntent(t, s, "intent-reserve")
	seedSignedOrder(t, s, "intent-reserve")

	ctx := context.Background()
	if _, err := s.ApplyFill(ctx, store.FillRecord{
		FillID: "fill-2", ExchangeOrderID: "intent-reserve-exchange", IntentID: "intent-reserve", UniqueTag: "lane-a",
		MarketID: "market", ConditionID: "condition", TokenID: "token", Outcome: "Up",
		Side: store.SideBuy, Shares: "10", Price: "0.5", TradeStatus: "CONFIRMED",
	}); err != nil {
		t.Fatalf("seed position: %v", err)
	}
	if err := s.Reserve(ctx, store.ReservationRecord{
		ReservationID: "reserve-1", IntentID: "intent-reserve", UniqueTag: "lane-a", ConditionID: "condition", TokenID: "token",
		Outcome: "Up", Side: store.SideSell, Shares: "3", Notional: "1.5", State: "active",
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	positions, err := s.PositionFeatures(ctx)
	if err != nil {
		t.Fatalf("positions after reserve: %v", err)
	}
	if positions[0].ReservedSize != "3" {
		t.Fatalf("expected reserved 3, got %+v", positions[0])
	}
	if err := s.Release(ctx, "reserve-1", "test release"); err != nil {
		t.Fatalf("release: %v", err)
	}
	positions, err = s.PositionFeatures(ctx)
	if err != nil {
		t.Fatalf("positions after release: %v", err)
	}
	if positions[0].ReservedSize != "0" {
		t.Fatalf("expected reserved 0 after release, got %+v", positions[0])
	}
}

func TestReserveEnforcesOpenBuyExposureLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	s.maxOpenBuyNotionalUSD = "10"
	t.Cleanup(func() { s.maxOpenBuyNotionalUSD = "" })

	seedIntent(t, s, "intent-exposure")
	within := store.ReservationRecord{
		ReservationID: "intent-exposure:1", IntentID: "intent-exposure", ChildSequence: 1, UniqueTag: "lane-a", MarketID: "market",
		ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy,
		Shares: "10", Notional: "8", State: "active",
	}
	if err := s.Reserve(ctx, within); err != nil {
		t.Fatalf("reserve within limit: %v", err)
	}
	t.Cleanup(func() { _ = s.Release(context.Background(), within.ReservationID, "cleanup") })

	beyond := within
	beyond.ReservationID = "intent-exposure:2"
	beyond.ChildSequence = 2
	beyond.Notional = "5"
	if err := s.Reserve(ctx, beyond); err != store.ErrExposureLimit {
		t.Fatalf("expected ErrExposureLimit, got %v", err)
	}
}
