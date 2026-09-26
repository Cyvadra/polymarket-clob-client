//go:build postgres

package postgres

import (
	"context"
	"fmt"
	"math/big"
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

// testLane is one test's private lane. The database is shared with earlier
// runs and with other tests, so every row a test writes is keyed by a tag
// unique to that run, read back by that tag, and deleted when the test ends.
type testLane struct {
	tag, conditionID, tokenID string
}

func newTestLane(t *testing.T, s *Store) testLane {
	t.Helper()
	suffix := fmt.Sprint(time.Now().UnixNano())
	lane := testLane{tag: "lane-" + suffix, conditionID: "condition-" + suffix, tokenID: "token-" + suffix}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, statement := range []string{
			`DELETE FROM reservations WHERE unique_tag = $1`,
			`DELETE FROM fills WHERE unique_tag = $1`,
			`DELETE FROM positions WHERE unique_tag = $1`,
			`DELETE FROM order_events WHERE intent_id IN (SELECT intent_id FROM order_intents WHERE unique_tag = $1)`,
			`DELETE FROM orders WHERE intent_id IN (SELECT intent_id FROM order_intents WHERE unique_tag = $1)`,
			`DELETE FROM order_intents WHERE unique_tag = $1`,
		} {
			if _, err := s.pool.Exec(ctx, statement, lane.tag); err != nil {
				t.Errorf("clean up lane %s: %v", lane.tag, err)
			}
		}
	})
	return lane
}

// id namespaces an intent, fill, or reservation ID to the lane.
func (l testLane) id(name string) string { return l.tag + "-" + name }

// position returns the lane's own row out of everything PositionFeatures reads.
func (l testLane) position(t *testing.T, s *Store) store.PositionRecord {
	t.Helper()
	positions, err := s.PositionFeatures(context.Background())
	if err != nil {
		t.Fatalf("positions: %v", err)
	}
	for _, position := range positions {
		if position.UniqueTag == l.tag {
			return position
		}
	}
	t.Fatalf("lane %s has no position row", l.tag)
	return store.PositionRecord{}
}

func seedIntent(t *testing.T, s *Store, lane testLane, intentID string) {
	t.Helper()
	_, err := s.InsertIntent(context.Background(), store.OrderIntentRecord{
		IntentID: intentID, UniqueTag: lane.tag, Strategy: "test", Kind: store.IntentOpen,
		MarketID: "market", ConditionID: lane.conditionID, TokenID: lane.tokenID, Outcome: "Up", Side: store.SideBuy,
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
	lane := newTestLane(t, s)
	intentID := lane.id("intent")
	seedIntent(t, s, lane, intentID)
	seedSignedOrder(t, s, intentID)

	first, err := s.TransitionOrder(context.Background(), store.SignedOrderRecord{
		IntentID: intentID, ChildSequence: 1, State: statemachine.StateSigned, Revision: 1, MatchedShares: "0",
	}, statemachine.EventSubmitStarted, "0", "", "submit")
	if err != nil {
		t.Fatalf("first transition: %v", err)
	}
	if first.State != statemachine.StateSubmitting {
		t.Fatalf("expected SUBMITTING, got %s", first.State)
	}

	_, err = s.TransitionOrder(context.Background(), store.SignedOrderRecord{
		IntentID: intentID, ChildSequence: 1, State: statemachine.StateSigned, Revision: 1, MatchedShares: "0",
	}, statemachine.EventSubmitStarted, "0", "", "submit again")
	if err != store.ErrConflict {
		t.Fatalf("expected ErrConflict on stale revision, got %v", err)
	}
}

func TestIntentStatusMirrorsOrderLifecycle(t *testing.T) {
	s := testStore(t)
	lane := newTestLane(t, s)
	intentID := lane.id("intent")
	seedIntent(t, s, lane, intentID)
	seedSignedOrder(t, s, intentID)

	_, err := s.TransitionOrder(context.Background(), store.SignedOrderRecord{
		IntentID: intentID, ChildSequence: 1, State: statemachine.StateSigned, Revision: 1, MatchedShares: "0",
	}, statemachine.EventSubmitStarted, "0", "", "submit")
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	intent, err := s.Intent(context.Background(), intentID)
	if err != nil {
		t.Fatalf("load intent: %v", err)
	}
	if intent.Status != statemachine.StateSubmitting {
		t.Fatalf("expected intent status SUBMITTING, got %s", intent.Status)
	}
}

func TestApplyFillThenFailedReversesPosition(t *testing.T) {
	s := testStore(t)
	lane := newTestLane(t, s)
	intentID := lane.id("intent")
	seedIntent(t, s, lane, intentID)
	seedSignedOrder(t, s, intentID)

	ctx := context.Background()
	inserted, err := s.ApplyFill(ctx, store.FillRecord{
		FillID: lane.id("fill-1"), ExchangeOrderID: intentID + "-exchange", IntentID: intentID, UniqueTag: lane.tag,
		MarketID: "market", ConditionID: lane.conditionID, TokenID: lane.tokenID, Outcome: "Up",
		Side: store.SideBuy, Shares: "10", Price: "0.5", TradeStatus: "CONFIRMED", TraderSide: "TAKER",
	})
	if err != nil || !inserted {
		t.Fatalf("apply fill: inserted=%v err=%v", inserted, err)
	}
	if position := lane.position(t, s); position.PositionSize != "10.000000000000000000" {
		t.Fatalf("expected the position to equal the reported fill size, got %+v", position)
	}

	reversed, err := s.ApplyFill(ctx, store.FillRecord{
		FillID: lane.id("fill-1"), ExchangeOrderID: intentID + "-exchange", IntentID: intentID, UniqueTag: lane.tag,
		MarketID: "market", ConditionID: lane.conditionID, TokenID: lane.tokenID, Outcome: "Up",
		Side: store.SideBuy, Shares: "10", Price: "0.5", TradeStatus: "FAILED", TraderSide: "TAKER",
	})
	if err != nil || !reversed {
		t.Fatalf("reverse fill: reversed=%v err=%v", reversed, err)
	}
	if position := lane.position(t, s); position.PositionSize != "0.000000000000000000" || position.AvailableSize != "0.000000000000000000" {
		t.Fatalf("expected fully reversed position, got %+v", position)
	}
}

// TestOpenLotsCountsBuyIntentsSinceEntry pins what open_lots means to the
// strategy that reads it: how many times the lane bought into the position it
// holds now. A partially filled open request is one lot however many fills it
// took, a reversed fill leaves no lot behind, and emptying the lane starts the
// count again — otherwise a strategy recovering a position after a restart
// would mistake a lane that is already full for one with room to add.
func TestOpenLotsCountsBuyIntentsSinceEntry(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	lane := newTestLane(t, s)
	fill := func(id, intent, side, shares, status string) {
		t.Helper()
		intentID := ""
		if intent != "" {
			intentID = lane.id(intent)
		}
		if _, err := s.ApplyFill(ctx, store.FillRecord{
			FillID: lane.id(id), IntentID: intentID, UniqueTag: lane.tag,
			MarketID: "market", ConditionID: lane.conditionID, TokenID: lane.tokenID, Outcome: "Up",
			Side: store.Side(side), Shares: shares, Price: "0.5", TradeStatus: status, TraderSide: "TAKER",
		}); err != nil {
			t.Fatalf("apply fill %s: %v", id, err)
		}
	}
	lots := func() int {
		t.Helper()
		return lane.position(t, s).OpenLots
	}

	// One open request filled in two parts is one lot.
	fill("a1", "intent-a", "BUY", "4", "CONFIRMED")
	fill("a2", "intent-a", "BUY", "6", "CONFIRMED")
	if got := lots(); got != 1 {
		t.Fatalf("open lots after one partially filled open = %d, want 1", got)
	}
	// A second open request adds a lot.
	fill("b1", "intent-b", "BUY", "5", "CONFIRMED")
	if got := lots(); got != 2 {
		t.Fatalf("open lots after a second open = %d, want 2", got)
	}
	// A fill the exchange later failed is reversed out of the position, so its
	// lot goes with it.
	fill("b1", "intent-b", "BUY", "5", "FAILED")
	if got := lots(); got != 1 {
		t.Fatalf("open lots after the second open was reversed = %d, want 1", got)
	}
	// Emptying the lane ends the episode; the next buy starts a fresh count.
	fill("s1", "", "SELL", "10", "CONFIRMED")
	if got := lots(); got != 0 {
		t.Fatalf("open lots of an emptied lane = %d, want 0", got)
	}
	fill("c1", "intent-c", "BUY", "3", "CONFIRMED")
	if got := lots(); got != 1 {
		t.Fatalf("open lots after the lane went flat and bought again = %d, want 1", got)
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

func TestOrderAveragePriceWeightsFillsAndIgnoresFailed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	priced, empty := "order-priced-"+suffix, "order-empty-"+suffix
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM fills WHERE exchange_order_id IN ($1, $2)`, priced, empty)
	})
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO fills (fill_id, exchange_order_id, unique_tag, condition_id, token_id, outcome, side, shares, price, trade_status, trader_side, received_at) VALUES
			($1 || '-a', $1, 'lane-avg', 'condition-avg', 'token-avg', 'Up', 'SELL', 3, 0.60, 'CONFIRMED', 'TAKER', now()),
			($1 || '-b', $1, 'lane-avg', 'condition-avg', 'token-avg', 'Up', 'SELL', 1, 0.80, 'CONFIRMED', 'TAKER', now()),
			($1 || '-c', $1, 'lane-avg', 'condition-avg', 'token-avg', 'Up', 'SELL', 5, 0.10, 'FAILED', 'TAKER', now())
	`, priced); err != nil {
		t.Fatalf("seed fills: %v", err)
	}

	price, err := s.OrderAveragePrice(ctx, priced)
	if err != nil {
		t.Fatalf("average price: %v", err)
	}
	// (3*0.60 + 1*0.80) / 4 = 0.65; the FAILED fill is excluded.
	// Compared as a number: the scale of the text is the division's, not ours.
	if got, ok := new(big.Rat).SetString(price); !ok || got.Cmp(big.NewRat(13, 20)) != 0 {
		t.Fatalf("expected weighted average 0.65, got %q", price)
	}

	price, err = s.OrderAveragePrice(ctx, empty)
	if err != nil {
		t.Fatalf("average price for order with no fills: %v", err)
	}
	if price != "" {
		t.Fatalf("expected empty string for an order with no fills, got %q", price)
	}
}

func TestReserveReleaseRestoresAvailableShares(t *testing.T) {
	s := testStore(t)
	lane := newTestLane(t, s)
	intentID := lane.id("intent")
	seedIntent(t, s, lane, intentID)
	seedSignedOrder(t, s, intentID)

	ctx := context.Background()
	if _, err := s.ApplyFill(ctx, store.FillRecord{
		FillID: lane.id("fill-2"), ExchangeOrderID: intentID + "-exchange", IntentID: intentID, UniqueTag: lane.tag,
		MarketID: "market", ConditionID: lane.conditionID, TokenID: lane.tokenID, Outcome: "Up",
		Side: store.SideBuy, Shares: "10", Price: "0.5", TradeStatus: "CONFIRMED",
	}); err != nil {
		t.Fatalf("seed position: %v", err)
	}
	if err := s.Reserve(ctx, store.ReservationRecord{
		ReservationID: lane.id("reserve-1"), IntentID: intentID, UniqueTag: lane.tag, ConditionID: lane.conditionID, TokenID: lane.tokenID,
		Outcome: "Up", Side: store.SideSell, Shares: "3", Notional: "1.5", State: "active",
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if position := lane.position(t, s); position.ReservedSize != "3.000000000000000000" {
		t.Fatalf("expected reserved 3, got %+v", position)
	}
	if err := s.Release(ctx, lane.id("reserve-1"), "test release"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if position := lane.position(t, s); position.ReservedSize != "0.000000000000000000" {
		t.Fatalf("expected reserved 0 after release, got %+v", position)
	}
}

func TestReserveEnforcesOpenBuyExposureLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	s.maxOpenBuyNotionalUSD = "10"
	t.Cleanup(func() { s.maxOpenBuyNotionalUSD = "" })

	lane := newTestLane(t, s)
	intentID := lane.id("intent")
	seedIntent(t, s, lane, intentID)
	within := store.ReservationRecord{
		ReservationID: intentID + ":1", IntentID: intentID, ChildSequence: 1, UniqueTag: lane.tag, MarketID: "market",
		ConditionID: lane.conditionID, TokenID: lane.tokenID, Outcome: "Up", Side: store.SideBuy,
		Shares: "10", Notional: "8", State: "active",
	}
	if err := s.Reserve(ctx, within); err != nil {
		t.Fatalf("reserve within limit: %v", err)
	}
	t.Cleanup(func() { _ = s.Release(context.Background(), within.ReservationID, "cleanup") })

	beyond := within
	beyond.ReservationID = intentID + ":2"
	beyond.ChildSequence = 2
	beyond.Notional = "5"
	if err := s.Reserve(ctx, beyond); err != store.ErrExposureLimit {
		t.Fatalf("expected ErrExposureLimit, got %v", err)
	}
}

// ReconcilePositionSize adopts the exchange's own view of a lane when it is
// smaller than the local fill ledger, and never inflates one: an unseen fill
// must arrive as a fill, not as a clamp.
func TestReconcilePositionSizeClampsDownOnly(t *testing.T) {
	s := testStore(t)
	lane := newTestLane(t, s)
	intentID := lane.id("intent")
	seedIntent(t, s, lane, intentID)
	seedSignedOrder(t, s, intentID)

	ctx := context.Background()
	if _, err := s.ApplyFill(ctx, store.FillRecord{
		FillID: lane.id("fill-clamp"), ExchangeOrderID: intentID + "-exchange", IntentID: intentID, UniqueTag: lane.tag,
		MarketID: "market", ConditionID: lane.conditionID, TokenID: lane.tokenID, Outcome: "Up",
		Side: store.SideBuy, Shares: "10", Price: "0.5", TradeStatus: "CONFIRMED",
	}); err != nil {
		t.Fatalf("seed position: %v", err)
	}
	if err := s.Reserve(ctx, store.ReservationRecord{
		ReservationID: lane.id("reserve-clamp"), IntentID: intentID, UniqueTag: lane.tag, ConditionID: lane.conditionID, TokenID: lane.tokenID,
		Outcome: "Up", Side: store.SideSell, Shares: "3", Notional: "1.5", State: "active",
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// A report above the local figure changes nothing.
	if err := s.ReconcilePositionSize(ctx, lane.conditionID, lane.tokenID, lane.tag, "12"); err != nil {
		t.Fatalf("reconcile upward: %v", err)
	}
	if position := lane.position(t, s); position.ActualShares != "10.000000000000000000" {
		t.Fatalf("expected an upward report to be ignored, got %+v", position)
	}

	// A report below it clamps the lane, keeping available + reserved <= total.
	if err := s.ReconcilePositionSize(ctx, lane.conditionID, lane.tokenID, lane.tag, "4"); err != nil {
		t.Fatalf("reconcile downward: %v", err)
	}
	got := lane.position(t, s)
	if got.ActualShares != "4.000000000000000000" || got.PositionSize != "4.000000000000000000" {
		t.Fatalf("expected the lane clamped to 4, got %+v", got)
	}
	if got.ReservedSize != "3.000000000000000000" || got.AvailableSize != "1.000000000000000000" {
		t.Fatalf("expected 3 reserved and 1 available after the clamp, got %+v", got)
	}
}

// SettlePosition ends a lane in a resolved market: it refuses a lane with
// shares reserved, in the statement itself, and emptying one clears its entry
// like a sell that empties it, so open_lots resets.
func TestSettlePositionSkipsReservedAndClearsAnEmptiedLane(t *testing.T) {
	s := testStore(t)
	lane := newTestLane(t, s)
	intentID := lane.id("intent")
	seedIntent(t, s, lane, intentID)
	seedSignedOrder(t, s, intentID)

	ctx := context.Background()
	if _, err := s.ApplyFill(ctx, store.FillRecord{
		FillID: lane.id("fill-settle"), ExchangeOrderID: intentID + "-exchange", IntentID: intentID, UniqueTag: lane.tag,
		MarketID: "market", ConditionID: lane.conditionID, TokenID: lane.tokenID, Outcome: "Up",
		Side: store.SideBuy, Shares: "10", Price: "0.5", TradeStatus: "CONFIRMED",
	}); err != nil {
		t.Fatalf("seed position: %v", err)
	}
	reservation := store.ReservationRecord{
		ReservationID: lane.id("reserve-settle"), IntentID: intentID, UniqueTag: lane.tag, ConditionID: lane.conditionID, TokenID: lane.tokenID,
		Outcome: "Up", Side: store.SideSell, Shares: "3", Notional: "1.5", State: "active",
	}
	if err := s.Reserve(ctx, reservation); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// A lane with a close working on it is left exactly as it was.
	if changed, err := s.SettlePosition(ctx, lane.conditionID, lane.tokenID, lane.tag, "0"); err != nil || changed {
		t.Fatalf("settle reserved lane: changed=%v err=%v, want it left alone", changed, err)
	}
	if got := lane.position(t, s); got.PositionSize != "10.000000000000000000" || got.OpenLots != 1 {
		t.Fatalf("expected the reserved lane untouched, got %+v", got)
	}
	if err := s.Release(ctx, reservation.ReservationID, "test"); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Shrinking keeps the entry; a report above the lane changes nothing.
	if changed, err := s.SettlePosition(ctx, lane.conditionID, lane.tokenID, lane.tag, "12"); err != nil || changed {
		t.Fatalf("settle upward: changed=%v err=%v", changed, err)
	}
	if changed, err := s.SettlePosition(ctx, lane.conditionID, lane.tokenID, lane.tag, "4"); err != nil || !changed {
		t.Fatalf("settle downward: changed=%v err=%v", changed, err)
	}
	got := lane.position(t, s)
	if got.PositionSize != "4.000000000000000000" || got.AvailableSize != "4.000000000000000000" || got.EntryTime.IsZero() || got.OpenLots != 1 {
		t.Fatalf("expected the lane shrunk to 4 with its entry kept, got %+v", got)
	}

	// Emptying clears the entry, so the lane counts no open lots.
	if changed, err := s.SettlePosition(ctx, lane.conditionID, lane.tokenID, lane.tag, "0"); err != nil || !changed {
		t.Fatalf("settle to zero: changed=%v err=%v", changed, err)
	}
	got = lane.position(t, s)
	if got.PositionSize != "0.000000000000000000" || got.State != "empty" || !got.EntryTime.IsZero() || got.EntryPrice != "" || got.OpenLots != 0 {
		t.Fatalf("expected an emptied lane with no entry, got %+v", got)
	}
}
