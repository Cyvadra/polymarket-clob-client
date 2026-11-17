//go:build postgres

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// inventoryLane is one test's strategy lane, its inventory lane, and the two
// tokens of its condition. Everything it writes is deleted when the test ends.
type inventoryLane struct {
	tag, inv, conditionID, up, down string
	// reserveErr, when set, takes preserve's reservation error.
	reserveErr *error
}

func newInventoryLane(t *testing.T, s *Store) inventoryLane {
	t.Helper()
	suffix := fmt.Sprint(time.Now().UnixNano())
	lane := inventoryLane{tag: "invlane-" + suffix, conditionID: "condition-" + suffix, up: "up-" + suffix, down: "down-" + suffix}
	lane.inv = inventoryTag(lane.tag)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, tag := range []string{lane.tag, lane.inv} {
			for _, statement := range []string{
				`DELETE FROM reservations WHERE unique_tag = $1`,
				`DELETE FROM fills WHERE unique_tag = $1`,
				`DELETE FROM positions WHERE unique_tag = $1`,
				`DELETE FROM order_intents WHERE unique_tag = $1`,
				`DELETE FROM ctf_operations WHERE unique_tag = $1`,
			} {
				if _, err := s.pool.Exec(ctx, statement, tag); err != nil {
					t.Errorf("clean up %s: %v", tag, err)
				}
			}
		}
	})
	return lane
}

func (l inventoryLane) op(id string) store.CTFOperationRecord {
	return store.CTFOperationRecord{
		OpID: l.tag + "-" + id, Strategy: "split-test", UniqueTag: l.tag, ConditionID: l.conditionID,
		Tokens: []string{l.up, l.down}, Outcomes: []string{"Up", "Down"},
	}
}

func (l inventoryLane) split(t *testing.T, s *Store, id, target, maxShares string) (store.CTFOperationRecord, bool) {
	t.Helper()
	op := l.op(id)
	op.TargetShares = target
	recorded, _, ok, err := s.BeginSplit(context.Background(), op, maxShares)
	if err != nil {
		t.Fatalf("begin split %s: %v", id, err)
	}
	if ok {
		if err := s.ConfirmCTFOperation(context.Background(), recorded.OpID, "0xhash", time.Now()); err != nil {
			t.Fatalf("confirm split %s: %v", id, err)
		}
	}
	return recorded, ok
}

func (l inventoryLane) level(t *testing.T, s *Store) store.InventoryLevel {
	t.Helper()
	level, err := s.InventoryLevel(context.Background(), l.tag, l.conditionID)
	if err != nil {
		t.Fatalf("inventory level: %v", err)
	}
	return level
}

func lanePosition(t *testing.T, s *Store, conditionID, tokenID, tag string) store.PositionRecord {
	t.Helper()
	position, found, err := s.LanePosition(context.Background(), conditionID, tokenID, tag)
	if err != nil {
		t.Fatalf("read lane %s %s: %v", tag, tokenID, err)
	}
	if !found {
		return store.PositionRecord{PositionSize: "0", OpenLots: 0}
	}
	return position
}

func requireDecimal(t *testing.T, what, got, want string) {
	t.Helper()
	if decimal.Compare(got, want) != 0 {
		t.Fatalf("%s = %s, want %s", what, got, want)
	}
}

// preserve records a SPLIT_PRESERVE intent buying shares of Up at price, as
// the executor stores it: a SELL of Down on the inventory lane, with its
// reservation.
func (l inventoryLane) preserve(t *testing.T, s *Store, id, shares, price string) string {
	t.Helper()
	ctx := context.Background()
	intentID := l.tag + "-" + id
	sellPrice, _ := decimal.SubString("1", price)
	policy, _ := json.Marshal(map[string]any{"style": "SPLIT_PRESERVE", "preserve": map[string]string{
		"unique_tag": l.tag, "token_id": l.up, "outcome": "Up", "limit_price": price, "requested_shares": shares, "shares": shares,
	}})
	if _, err := s.InsertIntent(ctx, store.OrderIntentRecord{
		IntentID: intentID, UniqueTag: l.inv, Strategy: "split-test", Kind: store.IntentOpen,
		ConditionID: l.conditionID, TokenID: l.down, Outcome: "Down", Side: store.SideSell, TargetUSD: "1",
		LimitPrice: sellPrice, TimeInForce: store.TimeInForceGTC, Status: statemachine.StateIntentReceived, Policy: policy,
		ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatalf("insert preserve intent: %v", err)
	}
	if err := s.Reserve(ctx, store.ReservationRecord{
		ReservationID: intentID + ":1", IntentID: intentID, ChildSequence: 1, ConditionID: l.conditionID, TokenID: l.down,
		UniqueTag: l.inv, Outcome: "Down", Side: "SELL", Shares: shares, Notional: "0",
	}); err != nil {
		if l.reserveErr != nil {
			*l.reserveErr = err
			return intentID
		}
		t.Fatalf("reserve preserve %s: %v", id, err)
	}
	return intentID
}

// reserveOn places a preserve and returns its reservation's error.
func reserveOn(t *testing.T, s *Store, l inventoryLane, id, shares string) error {
	t.Helper()
	var err error
	l.reserveErr = &err
	l.preserve(t, s, id, shares, "0.5")
	return err
}

func (l inventoryLane) fill(t *testing.T, s *Store, intentID, fillID, shares, price, fee, status string) {
	t.Helper()
	if _, err := s.ApplyFill(context.Background(), store.FillRecord{
		FillID: l.tag + "-" + fillID, ExchangeOrderID: intentID + "-x", IntentID: intentID, UniqueTag: l.inv,
		ConditionID: l.conditionID, TokenID: l.down, Outcome: "Down", Side: store.SideSell,
		Shares: shares, Price: price, Fee: fee, TradeStatus: status,
	}); err != nil {
		t.Fatalf("apply preserve fill %s: %v", fillID, err)
	}
}

// laneCash is the trade cash of every fill on the lane's two tags.
func (l inventoryLane) cash(t *testing.T, s *Store, tag string) string {
	t.Helper()
	var cash string
	if err := s.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(CASE WHEN side = 'SELL' THEN shares * price ELSE -shares * price END - fee), 0)::text
		FROM fills WHERE unique_tag = $1 AND trade_status <> 'FAILED'
	`, tag).Scan(&cash); err != nil {
		t.Fatalf("lane cash: %v", err)
	}
	return cash
}

func TestSplitPreserveRefillMergeNetsInventoryToZero(t *testing.T) {
	s := testStore(t)
	lane := newInventoryLane(t, s)

	if _, ok := lane.split(t, s, "s1", "100", ""); !ok {
		t.Fatal("first split recorded nothing")
	}
	level := lane.level(t, s)
	requireDecimal(t, "remaining after split", level.Remaining, "100")
	requireDecimal(t, "split cash", lane.cash(t, s, lane.inv), "-100")

	// Resending the same target splits nothing.
	if _, ok := lane.split(t, s, "s1-again", "100", ""); ok {
		t.Fatal("a split already at target recorded an operation")
	}

	// Preserve 60 Up: sell 60 Down at 0.4 with a 0.6 fee.
	first := lane.preserve(t, s, "p1", "60", "0.6")
	requireDecimal(t, "remaining with a resting preserve", lane.level(t, s).Remaining, "40")
	lane.fill(t, s, first, "f1", "60", "0.4", "0.6", "CONFIRMED")
	up := lanePosition(t, s, lane.conditionID, lane.up, lane.tag)
	requireDecimal(t, "strategy Up", up.PositionSize, "60")
	// 1 - 0.4 + 0.6/60
	requireDecimal(t, "strategy Up entry", up.EntryPrice, "0.61")
	if up.OpenLots != 1 {
		t.Fatalf("strategy Up open lots = %d, want 1", up.OpenLots)
	}
	requireDecimal(t, "remaining after the fill", lane.level(t, s).Remaining, "40")

	// Refill to 100 splits exactly what the entry used.
	refill, ok := lane.split(t, s, "s2", "100", "")
	if !ok {
		t.Fatal("refill recorded nothing")
	}
	requireDecimal(t, "refill shares", refill.Shares, "60")

	// A preserve larger than the inventory is held to it by the caller; the
	// store refuses a reservation the inventory cannot back.
	err := s.Reserve(context.Background(), store.ReservationRecord{
		ReservationID: lane.tag + "-too-big:1", IntentID: lane.tag + "-too-big", ChildSequence: 1, ConditionID: lane.conditionID,
		TokenID: lane.down, UniqueTag: lane.inv, Outcome: "Down", Side: "SELL", Shares: "101", Notional: "0",
	})
	if !errors.Is(err, store.ErrInsufficientInventory) {
		t.Fatalf("reserving past the inventory: %v, want ErrInsufficientInventory", err)
	}

	// A second preserve rests beside nothing else and fills.
	second := lane.preserve(t, s, "p2", "100", "0.1")
	lane.fill(t, s, second, "f2", "100", "0.9", "0", "CONFIRMED")
	requireDecimal(t, "remaining after both entries", lane.level(t, s).Remaining, "0")

	// Nothing left: the merge records nothing. Every pair split has left the
	// inventory, which nets to zero: -160 split, +23.4 and +90 sold, +36.6
	// and +10 moved to the strategy lane, which paid what two BUYs would.
	if _, ok, err := s.BeginMerge(context.Background(), lane.op("m1")); err != nil || ok {
		t.Fatalf("merge of an empty inventory: ok=%v err=%v", ok, err)
	}
	requireDecimal(t, "inventory lane cash", lane.cash(t, s, lane.inv), "0")
	requireDecimal(t, "strategy lane cash", lane.cash(t, s, lane.tag), "-46.6")
	for _, token := range []string{lane.up, lane.down} {
		requireDecimal(t, "inventory "+token, lanePosition(t, s, lane.conditionID, token, lane.inv).PositionSize, "0")
	}
	requireDecimal(t, "strategy Up total", lanePosition(t, s, lane.conditionID, lane.up, lane.tag).PositionSize, "160")
}

func TestMergeReturnsPairsAndClosesTheLane(t *testing.T) {
	s := testStore(t)
	lane := newInventoryLane(t, s)
	lane.split(t, s, "s1", "50", "")
	op := lane.op("m1")
	op.Shares = "20"
	merge, ok, err := s.BeginMerge(context.Background(), op)
	if err != nil || !ok {
		t.Fatalf("begin merge: ok=%v err=%v", ok, err)
	}
	if err := s.ConfirmCTFOperation(context.Background(), merge.OpID, "", time.Now()); err != nil {
		t.Fatalf("confirm merge: %v", err)
	}
	// Booking it twice changes nothing.
	if err := s.ConfirmCTFOperation(context.Background(), merge.OpID, "", time.Now()); err != nil {
		t.Fatalf("confirm merge again: %v", err)
	}
	level := lane.level(t, s)
	requireDecimal(t, "remaining after merge", level.Remaining, "30")
	requireDecimal(t, "inventory cash", lane.cash(t, s, lane.inv), "-30")
	if !level.Merged {
		t.Fatal("lane not marked merged")
	}
	op = lane.op("s2")
	op.TargetShares = "50"
	if _, _, _, err := s.BeginSplit(context.Background(), op, ""); !errors.Is(err, store.ErrInventoryClosed) {
		t.Fatalf("split after merge: %v, want ErrInventoryClosed", err)
	}
}

func TestSplitCountsInFlightAndCapsToCash(t *testing.T) {
	s := testStore(t)
	lane := newInventoryLane(t, s)
	op := lane.op("s1")
	op.TargetShares = "100"
	first, gap, ok, err := s.BeginSplit(context.Background(), op, "30")
	if err != nil || !ok {
		t.Fatalf("begin capped split: ok=%v err=%v", ok, err)
	}
	requireDecimal(t, "capped shares", first.Shares, "30")
	requireDecimal(t, "gap", gap, "100")
	// Not booked yet: a second split counts it as in flight.
	op = lane.op("s2")
	op.TargetShares = "100"
	second, _, ok, err := s.BeginSplit(context.Background(), op, "")
	if err != nil || !ok {
		t.Fatalf("begin second split: ok=%v err=%v", ok, err)
	}
	requireDecimal(t, "second split", second.Shares, "70")
	requireDecimal(t, "in flight", lane.level(t, s).InFlight, "100")
	// An unknown outcome still counts; a failure does not.
	if err := s.MarkCTFOperation(context.Background(), first.OpID, store.CTFUnknown, "", "", "lost"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkCTFOperation(context.Background(), second.OpID, store.CTFFailed, "", "", "refused"); err != nil {
		t.Fatal(err)
	}
	requireDecimal(t, "in flight after one failed", lane.level(t, s).InFlight, "30")
	settling, err := s.CTFOperationsSettling(context.Background(), time.Now())
	if err != nil || !settling {
		t.Fatalf("settling = %v, %v; want true", settling, err)
	}
}

func TestFailedPreserveFillReversesItsTransfer(t *testing.T) {
	s := testStore(t)
	lane := newInventoryLane(t, s)
	lane.split(t, s, "s1", "10", "")
	intentID := lane.preserve(t, s, "p1", "10", "0.5")
	lane.fill(t, s, intentID, "f1", "10", "0.5", "0", "MATCHED")
	requireDecimal(t, "strategy Up while matched", lanePosition(t, s, lane.conditionID, lane.up, lane.tag).PositionSize, "10")
	lane.fill(t, s, intentID, "f1", "10", "0.5", "0", "FAILED")
	requireDecimal(t, "strategy Up after failure", lanePosition(t, s, lane.conditionID, lane.up, lane.tag).PositionSize, "0")
	requireDecimal(t, "inventory Up after failure", lanePosition(t, s, lane.conditionID, lane.up, lane.inv).PositionSize, "10")
	requireDecimal(t, "inventory Down after failure", lanePosition(t, s, lane.conditionID, lane.down, lane.inv).PositionSize, "10")
	requireDecimal(t, "inventory cash after failure", lane.cash(t, s, lane.inv), "-10")
}

func TestMergeInFlightIsNotFreeInventory(t *testing.T) {
	s := testStore(t)
	lane := newInventoryLane(t, s)
	lane.split(t, s, "s1", "10", "")
	merge, ok, err := s.BeginMerge(context.Background(), lane.op("m1"))
	if err != nil || !ok {
		t.Fatalf("begin merge: ok=%v err=%v", ok, err)
	}
	requireDecimal(t, "merge shares", merge.Shares, "10")
	// The merge has not landed: its pairs are spoken for.
	requireDecimal(t, "remaining with a merge in flight", lane.level(t, s).Remaining, "0")
	err = s.Reserve(context.Background(), store.ReservationRecord{
		ReservationID: lane.tag + "-r1", IntentID: lane.tag + "-none", ChildSequence: 1, ConditionID: lane.conditionID,
		TokenID: lane.down, UniqueTag: lane.inv, Outcome: "Down", Side: "SELL", Shares: "5", Notional: "0",
	})
	if !errors.Is(err, store.ErrInsufficientInventory) {
		t.Fatalf("preserve reserved against a merge in flight: %v", err)
	}
	if _, ok, err := s.BeginMerge(context.Background(), lane.op("m2")); err != nil || ok {
		t.Fatalf("second merge: ok=%v err=%v, want nothing to merge", ok, err)
	}
	// A failed merge gives its pairs back.
	if err := s.MarkCTFOperation(context.Background(), merge.OpID, store.CTFFailed, "", "", "refused"); err != nil {
		t.Fatal(err)
	}
	requireDecimal(t, "remaining after the merge failed", lane.level(t, s).Remaining, "10")
}

func TestEmptyMergeStillClosesTheLane(t *testing.T) {
	s := testStore(t)
	lane := newInventoryLane(t, s)
	if _, ok, err := s.BeginMerge(context.Background(), lane.op("m1")); err != nil || ok {
		t.Fatalf("merge of nothing: ok=%v err=%v", ok, err)
	}
	if !lane.level(t, s).Merged {
		t.Fatal("lane not marked merged")
	}
	op := lane.op("s1")
	op.TargetShares = "10"
	if _, _, _, err := s.BeginSplit(context.Background(), op, ""); !errors.Is(err, store.ErrInventoryClosed) {
		t.Fatalf("split after an empty merge: %v, want ErrInventoryClosed", err)
	}
}

func TestClosingMergeRefusesReservationsAndIsSizedInPlace(t *testing.T) {
	s := testStore(t)
	lane := newInventoryLane(t, s)
	ctx := context.Background()
	lane.split(t, s, "s1", "10", "")
	closing, err := s.CloseInventory(ctx, lane.op("m1"))
	if err != nil {
		t.Fatalf("close inventory: %v", err)
	}
	if !lane.level(t, s).Merged {
		t.Fatal("a closing lane must read as merged")
	}
	op := lane.op("s2")
	op.TargetShares = "20"
	if _, _, _, err := s.BeginSplit(ctx, op, ""); !errors.Is(err, store.ErrInventoryClosed) {
		t.Fatalf("split on a closing lane: %v", err)
	}
	if err := reserveOn(t, s, lane, "p1", "1"); !errors.Is(err, store.ErrInsufficientInventory) {
		t.Fatalf("reserve on a closing lane: %v", err)
	}
	closing.Shares = "" // as the manager does: the request's shares, here all
	merge, ok, err := s.BeginMerge(ctx, closing)
	if err != nil || !ok {
		t.Fatalf("begin merge: %v %v", ok, err)
	}
	requireDecimal(t, "merge shares", merge.Shares, "10")
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ctf_operations WHERE unique_tag = $1 AND action = 'MERGE'`, lane.tag).Scan(&count); err != nil || count != 1 {
		t.Fatalf("merge rows = %d, %v; want the closing one sized in place", count, err)
	}
}

func TestSplitGapIsTruncatedLikeTheSplit(t *testing.T) {
	s := testStore(t)
	lane := newInventoryLane(t, s)
	op := lane.op("s1")
	op.TargetShares = "10.1234567"
	recorded, gap, ok, err := s.BeginSplit(context.Background(), op, "")
	if err != nil || !ok {
		t.Fatalf("begin split: %v %v", ok, err)
	}
	requireDecimal(t, "gap", gap, recorded.Shares)
	requireDecimal(t, "shares", recorded.Shares, "10.123456")
}
