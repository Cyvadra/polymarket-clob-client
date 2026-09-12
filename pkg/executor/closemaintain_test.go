package executor

import (
	"context"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// fakeQuotes serves one snapshot for every condition.
type fakeQuotes struct {
	snapshot marketquotes.Snapshot
	has      bool
}

func (q fakeQuotes) Get(string) (marketquotes.Snapshot, bool) { return q.snapshot, q.has }

// laneWithRestingClose models a take-profit resting for 6 shares on a lane
// whose position has since grown to 10.
func laneWithRestingClose(revision int64) *fakeStore {
	return &fakeStore{
		inserted: true,
		positions: []store.PositionRecord{{
			ConditionID: "condition", TokenID: "token", UniqueTag: "lane-a", Outcome: "Up",
			PositionSize: "10", ActualShares: "10", AvailableSize: "4", ReservedSize: "6",
			State: "open", SourceRevision: revision,
		}},
		order: store.SignedOrderRecord{
			IntentID: "close-1", ChildSequence: 1, ExchangeOrderID: "order-close",
			State: statemachine.StateLive, Revision: 1, RequestedShares: "6", MatchedShares: "0",
		},
		intent: store.OrderIntentRecord{
			IntentID: "close-1", UniqueTag: "lane-a", Strategy: "strategy", ConditionID: "condition",
			TokenID: "token", Outcome: "Up", Kind: store.IntentClose, Side: "SELL",
			LimitPrice: "0.55", TimeInForce: "GTC", Status: statemachine.StateLive,
		},
		// The resting close holds its 6 shares in reserve, which is why the
		// lane reports only 4 available against a position of 10.
		reservations: map[string]store.ReservationRecord{
			reservationID("close-1", 1): {
				ReservationID: reservationID("close-1", 1), IntentID: "close-1", ChildSequence: 1,
				UniqueTag: "lane-a", ConditionID: "condition", TokenID: "token", Outcome: "Up",
				Side: "SELL", Shares: "6", State: "active",
			},
		},
	}
}

// The first pass only records the lane's revision; a position that may still be
// filling is left alone until it holds still for a full tick.
func TestMaintainClosesWaitsForThePositionToSettle(t *testing.T) {
	storer := laneWithRestingClose(7)
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-new"}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	exec.SetEventPublisher(&resultPublisher{})
	if err := exec.maintainCloses(context.Background()); err != nil {
		t.Fatalf("first maintenance pass: %v", err)
	}
	if len(client.canceledOrderIDs) != 0 {
		t.Fatalf("expected no replacement on the first pass, got %+v", client.canceledOrderIDs)
	}
}

// Once the position stops moving, the resting close is replaced for the whole
// current position rather than for the shortfall, which would be unplaceable.
func TestMaintainClosesReplacesRestingCloseAtTheGrownSize(t *testing.T) {
	storer := laneWithRestingClose(7)
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-new"}, minOrderSize: 5}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	exec.SetEventPublisher(&resultPublisher{})
	if err := exec.maintainCloses(context.Background()); err != nil {
		t.Fatalf("first maintenance pass: %v", err)
	}
	if err := exec.maintainCloses(context.Background()); err != nil {
		t.Fatalf("second maintenance pass: %v", err)
	}
	if len(client.canceledOrderIDs) != 1 || client.canceledOrderIDs[0] != "order-close" {
		t.Fatalf("expected the stale close canceled, got %+v", client.canceledOrderIDs)
	}
	// available_size 4 plus the 6 the resting order holds in reserve.
	if client.created.Shares != 10 {
		t.Fatalf("expected the replacement to cover the whole position, got %v", client.created.Shares)
	}
}

// laneWithResidual models a late fill of 2 shares left after the close is gone,
// below a minimum order size of 5.
func laneWithResidual() *fakeStore {
	return &fakeStore{
		inserted: true,
		positions: []store.PositionRecord{{
			ConditionID: "condition", TokenID: "token", UniqueTag: "lane-a", Outcome: "Up",
			PositionSize: "2", ActualShares: "2", AvailableSize: "2", State: "open", SourceRevision: 3,
		}},
		latestClose: store.OrderIntentRecord{
			IntentID: "close-1", UniqueTag: "lane-a", Strategy: "strategy", ConditionID: "condition",
			TokenID: "token", Outcome: "Up", Kind: store.IntentClose, Side: "SELL",
			LimitPrice: "0.55", TimeInForce: "GTC",
		},
	}
}

func quotesAtBid(bid float64) fakeQuotes {
	return fakeQuotes{has: true, snapshot: marketquotes.Snapshot{
		ConditionID: "condition",
		Up:          marketquotes.Quote{AssetID: "token", Bid: bid, Ask: bid + 0.01},
	}}
}

// A residual under the market minimum cannot rest on the book, so nothing is
// placed until the bid comes up to the strategy's own limit price.
func TestMaintainClosesHoldsSubMinimumResidualBelowTheLimitPrice(t *testing.T) {
	storer := laneWithResidual()
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-new"}, minOrderSize: 5}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	exec.SetEventPublisher(&resultPublisher{})
	exec.SetQuoteProvider(quotesAtBid(0.51))
	if err := exec.maintainCloses(context.Background()); err != nil {
		t.Fatalf("maintenance pass: %v", err)
	}
	if client.submissions != 0 {
		t.Fatalf("expected no order while the bid is below the limit, got %d", client.submissions)
	}
}

// Once the bid reaches the limit price the residual is taken with a FAK, as an
// internal child that publishes no close result.
func TestMaintainClosesTakesSubMinimumResidualWhenTheBidReachesTheLimit(t *testing.T) {
	storer := laneWithResidual()
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-new"}, minOrderSize: 5}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	pub := &resultPublisher{}
	exec.SetEventPublisher(pub)
	exec.SetQuoteProvider(quotesAtBid(0.55))
	if err := exec.maintainCloses(context.Background()); err != nil {
		t.Fatalf("maintenance pass: %v", err)
	}
	if client.submissions != 1 {
		t.Fatalf("expected the residual taken once, got %d submissions", client.submissions)
	}
	if client.created.Shares != 2 || client.created.Price != 0.55 {
		t.Fatalf("expected 2 shares at the limit price, got %+v", client.created)
	}
	if client.created.OrderType != protocol.TimeInForceFAK {
		t.Fatalf("expected a FAK, got %q", client.created.OrderType)
	}
	if pub.subject == protocol.SubjectExecutionCloseResult {
		t.Fatalf("expected the residual exit to publish no close result, got %+v", pub.value)
	}
}

// A residual the exchange may keep refusing is offered again on an interval,
// not on every tick.
func TestMaintainClosesSpacesRepeatedResidualSweeps(t *testing.T) {
	storer := laneWithResidual()
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-new"}, minOrderSize: 5}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	exec.SetEventPublisher(&resultPublisher{})
	exec.SetQuoteProvider(quotesAtBid(0.55))
	for i := 0; i < 3; i++ {
		if err := exec.maintainCloses(context.Background()); err != nil {
			t.Fatalf("maintenance pass %d: %v", i, err)
		}
	}
	if client.submissions != 1 {
		t.Fatalf("expected the residual offered once within the interval, got %d submissions", client.submissions)
	}
}

// A position large enough to place but with no resting close is left to the
// strategy: re-placing it here would retry forever on a lane whose close the
// exchange keeps refusing, publishing a FAILED close result each time.
func TestMaintainClosesLeavesAPlaceablePositionToTheStrategy(t *testing.T) {
	storer := laneWithResidual()
	storer.positions[0].PositionSize = "20"
	storer.positions[0].ActualShares = "20"
	storer.positions[0].AvailableSize = "20"
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-new"}, minOrderSize: 5}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	exec.SetEventPublisher(&resultPublisher{})
	exec.SetQuoteProvider(quotesAtBid(0.55))
	if err := exec.maintainCloses(context.Background()); err != nil {
		t.Fatalf("maintenance pass: %v", err)
	}
	if client.submissions != 0 {
		t.Fatalf("expected no order placed, got %d submissions", client.submissions)
	}
}
