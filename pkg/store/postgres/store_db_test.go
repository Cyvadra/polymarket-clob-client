//go:build postgres

package postgres

import (
	"context"
	"fmt"
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
	if len(positions) != 1 || positions[0].PositionSize != "10.000000000000000000" {
		t.Fatalf("expected the position to equal the reported fill size, got %+v", positions)
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
	if len(positions) != 1 || positions[0].PositionSize != "0.000000000000000000" || positions[0].AvailableSize != "0.000000000000000000" {
		t.Fatalf("expected fully reversed position, got %+v", positions)
	}
}

func TestPositionsFromFillsMigrationRebuildsSizesFromFills(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	partial, bought := "lane-partial-"+suffix, "lane-bought-"+suffix
	// Its own market keeps these rows out of the other tests, which share the
	// database and read positions[0] of the "condition"/"token" market.
	const conditionID, tokenID = "condition-rebuild", "token-rebuild"
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM fills WHERE unique_tag IN ($1, $2)`, partial, bought)
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM positions WHERE unique_tag IN ($1, $2)`, partial, bought)
	})
	// Both lanes as the old 7% taker-fee model left them: one credited
	// 5.280366 for a 5.46 buy and then sold 5.28, one credited 2.2693708 for a
	// 2.44 buy and never given an entry price.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO fills (fill_id, unique_tag, condition_id, token_id, outcome, side, shares, price, trade_status, trader_side, received_at) VALUES
			($1 || '-buy', $1, $3, $4, 'Down', 'BUY', 5.46, 0.53, 'CONFIRMED', 'TAKER', now() - interval '1 minute'),
			($1 || '-sell', $1, $3, $4, 'Down', 'SELL', 5.28, 0.54, 'CONFIRMED', 'TAKER', now()),
			($2 || '-buy', $2, $3, $4, 'Up', 'BUY', 2.44, 0.001, 'CONFIRMED', 'TAKER', now())
	`, partial, bought, conditionID, tokenID); err != nil {
		t.Fatalf("seed fills: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO positions (condition_id, token_id, unique_tag, outcome, position_size, actual_shares, available_size, entry_price, entry_time, state, source_revision) VALUES
			($3, $4, $1, 'Down', 0.000366, 0.000366, 0.000366, 0.53, now(), 'open', 4),
			($3, $4, $2, 'Up', 2.2693708, 2.2693708, 2.2693708, NULL, NULL, 'open', 1)
	`, partial, bought, conditionID, tokenID); err != nil {
		t.Fatalf("seed positions: %v", err)
	}
	migrations, err := Migrations()
	if err != nil {
		t.Fatalf("migrations: %v", err)
	}
	var rebuild string
	for _, migration := range migrations {
		if migration.Name == "000002_positions_from_fills.sql" {
			rebuild = migration.SQL
		}
	}
	if rebuild == "" {
		t.Fatal("position rebuild migration not embedded")
	}
	if _, err := s.pool.Exec(ctx, rebuild); err != nil {
		t.Fatalf("run position rebuild: %v", err)
	}
	positions, err := s.PositionFeatures(ctx)
	if err != nil {
		t.Fatalf("positions: %v", err)
	}
	want := map[string]struct{ size, entry string }{
		partial: {"0.180000000000000000", "0.530000000000000000"},
		bought:  {"2.440000000000000000", "0.001000000000000000"},
	}
	for _, position := range positions {
		expected, ok := want[position.UniqueTag]
		if !ok {
			continue
		}
		delete(want, position.UniqueTag)
		if position.PositionSize != expected.size || position.ActualShares != expected.size || position.AvailableSize != expected.size ||
			position.EntryPrice != expected.entry || position.State != "open" {
			t.Fatalf("lane %s not rebuilt from fills: %+v", position.UniqueTag, position)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing rebuilt lanes: %v", want)
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
	if positions[0].ReservedSize != "3.000000000000000000" {
		t.Fatalf("expected reserved 3, got %+v", positions[0])
	}
	if err := s.Release(ctx, "reserve-1", "test release"); err != nil {
		t.Fatalf("release: %v", err)
	}
	positions, err = s.PositionFeatures(ctx)
	if err != nil {
		t.Fatalf("positions after release: %v", err)
	}
	if positions[0].ReservedSize != "0.000000000000000000" {
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
