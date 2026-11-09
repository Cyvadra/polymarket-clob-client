//go:build postgres

package postgres

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

func seedLadderIntent(t *testing.T, s *Store, lane testLane, intentID string) {
	t.Helper()
	_, err := s.InsertIntent(context.Background(), store.OrderIntentRecord{
		IntentID: intentID, UniqueTag: lane.tag, Strategy: "test", Kind: store.IntentOpen,
		MarketID: "market", ConditionID: lane.conditionID, TokenID: lane.tokenID, Outcome: "Up", Side: store.SideBuy,
		TargetUSD: "12", LimitPrice: "0.60", BottomPrice: "0.50", ScalingMode: "flat", TimeInForce: store.TimeInForceGTC,
		PostOnly: true, Status: statemachine.StateIntentReceived,
	})
	if err != nil {
		t.Fatalf("seed ladder intent: %v", err)
	}
}

// salt reads the salt out of a stored payload; the column is jsonb, which
// normalises whitespace, so payloads are compared by content.
func salt(t *testing.T, payload []byte) int {
	t.Helper()
	var decoded struct {
		Salt int `json:"salt"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("stored payload %q is not JSON: %v", payload, err)
	}
	return decoded.Salt
}

func persistChild(t *testing.T, s *Store, intentID string, sequence int) store.SignedOrderRecord {
	t.Helper()
	name := intentID + "-" + string(rune('a'+sequence))
	submitting, err := s.PersistSubmittingOrder(context.Background(), store.SignedOrderRecord{
		IntentID: intentID, ChildSequence: sequence, SignedPayload: []byte(`{"salt":1}`), SignedOrderHash: name + "-hash", Salt: "1",
		ExchangeOrderID: name + "-exchange", RequestedShares: "20", Price: "0.6", OrderType: store.TimeInForceGTC, PostOnly: true,
		State: statemachine.StateSigned, Revision: 1, MatchedShares: "0",
	}, "submit")
	if err != nil {
		t.Fatalf("persist child %d: %v", sequence, err)
	}
	return submitting
}

func TestLadderIntentRoundTripsItsRangeAndStartsWithoutAPhase(t *testing.T) {
	s := testStore(t)
	lane := newTestLane(t, s)
	intentID := lane.id("ladder")
	seedLadderIntent(t, s, lane, intentID)
	intent, err := s.Intent(context.Background(), intentID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.BottomPrice == "" || intent.ScalingMode != "flat" {
		t.Fatalf("intent lost its ladder fields: %+v", intent)
	}
	if phase, err := s.LadderPhase(context.Background(), intentID); err != nil || phase != store.LadderNone {
		t.Fatalf("phase=%q err=%v, want none", phase, err)
	}
	if _, err := s.LadderPhase(context.Background(), lane.id("missing")); err != store.ErrNotFound {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
}

func TestLadderRangeConstraintRefusesBottomAboveLimit(t *testing.T) {
	s := testStore(t)
	lane := newTestLane(t, s)
	_, err := s.InsertIntent(context.Background(), store.OrderIntentRecord{
		IntentID: lane.id("bad"), UniqueTag: lane.tag, Strategy: "test", Kind: store.IntentOpen, MarketID: "m",
		ConditionID: lane.conditionID, TokenID: lane.tokenID, Outcome: "Up", Side: store.SideBuy, TargetUSD: "1",
		LimitPrice: "0.40", BottomPrice: "0.50", TimeInForce: store.TimeInForceGTC, Status: statemachine.StateIntentReceived,
	})
	if err == nil {
		t.Fatal("a bottom_price above limit_price was stored")
	}
}

func TestMoveLadderPhaseIsACompareAndSet(t *testing.T) {
	s := testStore(t)
	lane := newTestLane(t, s)
	intentID := lane.id("ladder")
	seedLadderIntent(t, s, lane, intentID)
	ctx := context.Background()

	if moved, err := s.MoveLadderPhase(ctx, intentID, store.LadderActive, store.LadderPlacing); err != nil || moved {
		t.Fatalf("moved=%v err=%v, want no move from a phase it is not in", moved, err)
	}
	if moved, err := s.MoveLadderPhase(ctx, intentID, store.LadderPlacing, store.LadderNone); err != nil || !moved {
		t.Fatalf("moved=%v err=%v, want NONE to PLACING", moved, err)
	}
	if moved, err := s.MoveLadderPhase(ctx, intentID, store.LadderStopping, store.LadderPlacing, store.LadderActive); err != nil || !moved {
		t.Fatalf("moved=%v err=%v, want PLACING to STOPPING", moved, err)
	}
	if phase, _ := s.LadderPhase(ctx, intentID); phase != store.LadderStopping {
		t.Fatalf("phase=%s", phase)
	}
	if _, err := s.MoveLadderPhase(ctx, intentID, store.LadderNone, store.LadderActive); err == nil {
		t.Fatal("moving to no phase was allowed")
	}
}

func TestMoveLadderPhaseLetsExactlyOneClaimantWin(t *testing.T) {
	s := testStore(t)
	lane := newTestLane(t, s)
	intentID := lane.id("ladder")
	seedLadderIntent(t, s, lane, intentID)
	ctx := context.Background()
	if _, err := s.MoveLadderPhase(ctx, intentID, store.LadderActive, store.LadderNone); err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			moved, err := s.MoveLadderPhase(ctx, intentID, store.LadderPublished, store.LadderActive, store.LadderStopping)
			if err != nil {
				t.Error(err)
			}
			if moved {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("%d claimants won, want exactly 1", winners.Load())
	}
}

func TestChildTransitionsLeaveTheLadderPhaseAlone(t *testing.T) {
	s := testStore(t)
	lane := newTestLane(t, s)
	intentID := lane.id("ladder")
	seedLadderIntent(t, s, lane, intentID)
	ctx := context.Background()
	if _, err := s.MoveLadderPhase(ctx, intentID, store.LadderStopping, store.LadderNone); err != nil {
		t.Fatal(err)
	}
	child := persistChild(t, s, intentID, 1)
	if _, err := s.TransitionOrder(ctx, child, statemachine.EventSubmitAcknowledged, "0", "", "ack"); err != nil {
		t.Fatal(err)
	}
	intent, _ := s.Intent(ctx, intentID)
	if intent.Status != statemachine.StateLive {
		t.Fatalf("status=%s: the status follows the child, which is why the phase has its own column", intent.Status)
	}
	if phase, _ := s.LadderPhase(ctx, intentID); phase != store.LadderStopping {
		t.Fatalf("phase=%s, a child transition rewrote it", phase)
	}
}

func TestIntentOrdersListsEveryChildLowestSequenceFirst(t *testing.T) {
	s := testStore(t)
	lane := newTestLane(t, s)
	intentID := lane.id("ladder")
	seedLadderIntent(t, s, lane, intentID)
	for _, sequence := range []int{3, 1, 2} {
		persistChild(t, s, intentID, sequence)
	}
	orders, err := s.IntentOrders(context.Background(), intentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 3 || orders[0].ChildSequence != 1 || orders[1].ChildSequence != 2 || orders[2].ChildSequence != 3 {
		t.Fatalf("orders=%+v, want sequences 1,2,3", orders)
	}
	if orders, err := s.IntentOrders(context.Background(), lane.id("none")); err != nil || len(orders) != 0 {
		t.Fatalf("orders=%v err=%v for an intent with no children", orders, err)
	}
}

func TestResignRetryOrderSwapsTheSignatureAndReentersSubmitting(t *testing.T) {
	s := testStore(t)
	lane := newTestLane(t, s)
	intentID := lane.id("ladder")
	seedLadderIntent(t, s, lane, intentID)
	ctx := context.Background()
	child := persistChild(t, s, intentID, 1)
	parked, err := s.TransitionOrder(ctx, child, statemachine.EventPostOnlyCross, "0", "", "crossed")
	if err != nil || parked.State != statemachine.StateRetryPending {
		t.Fatalf("parked=%+v err=%v", parked, err)
	}

	resigned, err := s.ResignRetryOrder(ctx, parked, []byte(`{"salt":2}`), intentID+"-hash-2", "2", intentID+"-exchange-2", "retry")
	if err != nil {
		t.Fatal(err)
	}
	if resigned.State != statemachine.StateSubmitting || resigned.ExchangeOrderID != intentID+"-exchange-2" || resigned.Salt != "2" {
		t.Fatalf("resigned=%+v", resigned)
	}
	stored, err := s.OrderByIntent(ctx, intentID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if salt(t, stored.SignedPayload) != 2 || stored.SignedOrderHash != intentID+"-hash-2" || stored.Salt != "2" || stored.ExchangeOrderID != intentID+"-exchange-2" || stored.Revision != resigned.Revision {
		t.Fatalf("stored=%+v does not carry the new signature", stored)
	}
	// The same parked row cannot be re-signed twice: the revision moved.
	if _, err := s.ResignRetryOrder(ctx, parked, []byte(`{"salt":3}`), intentID+"-hash-3", "3", intentID+"-exchange-3", "again"); err != store.ErrConflict {
		t.Fatalf("err=%v, want ErrConflict for a stale revision", err)
	}
	after, _ := s.OrderByIntent(ctx, intentID, 1)
	if salt(t, after.SignedPayload) != 2 {
		t.Fatalf("a refused re-sign still changed the payload: %s", after.SignedPayload)
	}
	// An order that is not parked is refused without touching it.
	if _, err := s.ResignRetryOrder(ctx, resigned, []byte(`{"salt":4}`), intentID+"-hash-4", "4", intentID+"-exchange-4", "x"); err == nil {
		t.Fatal("a SUBMITTING order was re-signed")
	}
	if _, err := s.ResignRetryOrder(ctx, parked, nil, "", "", "", ""); err == nil {
		t.Fatal("an empty signature was accepted")
	}
}

func TestParkedOrderReservationSurvivesAndCancelReleasesIt(t *testing.T) {
	s := testStore(t)
	lane := newTestLane(t, s)
	intentID := lane.id("ladder")
	seedLadderIntent(t, s, lane, intentID)
	ctx := context.Background()
	if err := s.Reserve(ctx, store.ReservationRecord{
		ReservationID: intentID + ":1", IntentID: intentID, ChildSequence: 1, UniqueTag: lane.tag, MarketID: "market",
		ConditionID: lane.conditionID, TokenID: lane.tokenID, Outcome: "Up", Side: store.SideBuy, Shares: "20", Notional: "12", Reason: "ladder",
	}); err != nil {
		t.Fatal(err)
	}
	child := persistChild(t, s, intentID, 1)
	parked, err := s.TransitionOrder(ctx, child, statemachine.EventPostOnlyCross, "0", "", "crossed")
	if err != nil {
		t.Fatal(err)
	}
	if reservation, err := s.Reservation(ctx, intentID+":1"); err != nil || reservation.State != "active" {
		t.Fatalf("reservation=%+v err=%v, want it held while parked", reservation, err)
	}
	if _, err := s.TransitionOrder(ctx, parked, statemachine.EventCancelObserved, "0", "", "cancel"); err != nil {
		t.Fatal(err)
	}
	if reservation, err := s.Reservation(ctx, intentID+":1"); err == nil && reservation.State == "active" {
		t.Fatalf("reservation still active after the parked order was cancelled: %+v", reservation)
	}
}

func TestPendingLaddersListsOnlyThoseOwingAResult(t *testing.T) {
	s := testStore(t)
	lane := newTestLane(t, s)
	ctx := context.Background()
	for name, phase := range map[string]store.LadderPhase{"none": store.LadderNone, "placing": store.LadderPlacing, "active": store.LadderActive, "stopping": store.LadderStopping, "published": store.LadderPublished, "resting": store.LadderActive} {
		id := lane.id(name)
		seedLadderIntent(t, s, lane, id)
		if name == "resting" {
			// A level still open: the result is not due, so it is not listed.
			persistChild(t, s, id, 1)
		}
		if phase == store.LadderNone {
			continue
		}
		if _, err := s.MoveLadderPhase(ctx, id, phase, store.LadderNone); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := s.PendingLadders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got[lane.id("active")] || !got[lane.id("stopping")] {
		t.Fatalf("ids=%v, want the active and stopping ladders", ids)
	}
	for _, name := range []string{"none", "placing", "published", "resting"} {
		if got[lane.id(name)] {
			t.Fatalf("ladder %q is listed but owes no result", name)
		}
	}
}
