package executiontest

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
)

func TestOrderEventsBetweenFiltersByLaneTag(t *testing.T) {
	o := &Observer{}
	mine := protocol.ExecutionOrderEvent{IntentID: "a", UniqueTag: "lane-mine", State: "FILLED", MatchedShares: 5}
	theirs := protocol.ExecutionOrderEvent{IntentID: "b", UniqueTag: "lane-theirs", State: "FILLED", MatchedShares: 9}
	untagged := protocol.ExecutionOrderEvent{IntentID: "c", State: "LIVE"}
	for _, event := range []protocol.ExecutionOrderEvent{mine, theirs, untagged} {
		payload, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		o.record(protocol.SubjectExecutionOrderEvent, payload)
	}
	start := time.Now().Add(-time.Minute)
	got := o.OrderEventsBetween(start, time.Time{}, "lane-mine")
	if len(got) != 1 || got[0].IntentID != "a" {
		t.Fatalf("expected only the caller's lane, got %+v", got)
	}
	if all := o.OrderEventsBetween(start, time.Time{}, ""); len(all) != 3 {
		t.Fatalf("expected an empty tag to keep every event, got %d", len(all))
	}
}

// A partly filled order reports its cumulative matched_shares on every later
// transition, so each order counts once at its largest value.
func TestMatchedSharesCountsEachOrderOnce(t *testing.T) {
	events := []protocol.ExecutionOrderEvent{
		{IntentID: "open", State: "SUBMITTING"},
		{IntentID: "open", ExchangeOrderID: "a", State: "LIVE"},
		{IntentID: "open", ExchangeOrderID: "a", State: "PARTIALLY_FILLED", MatchedShares: 3},
		{IntentID: "open", ExchangeOrderID: "a", State: "FILLED", MatchedShares: 7.09},
		{IntentID: "close", ExchangeOrderID: "b", State: "FILLED", MatchedShares: 0.5},
	}
	total, orders := matchedShares(events)
	if math.Abs(total-7.59) > 1e-9 || orders != 2 {
		t.Fatalf("expected 7.59 shares across 2 orders, got %.8f across %d", total, orders)
	}
}

func TestResidualOrdersChecksEveryOrder(t *testing.T) {
	events := []protocol.ExecutionOrderEvent{
		{IntentID: "limit-close", State: "SUBMITTING"},
		{IntentID: "limit-close", ExchangeOrderID: "a", State: "LIVE"},
		{IntentID: "force-close", State: "SUBMITTING"},
		{IntentID: "force-close", ExchangeOrderID: "b", State: "FILLED", MatchedShares: 7.5},
	}
	residual := residualOrders(events)
	if len(residual) != 1 || residual[0].IntentID != "limit-close" {
		t.Fatalf("expected only the resting limit close to be residual, got %+v", residual)
	}
	events = append(events, protocol.ExecutionOrderEvent{IntentID: "limit-close", ExchangeOrderID: "a", State: "CANCELED"})
	if residual := residualOrders(events); len(residual) != 0 {
		t.Fatalf("expected no residual order once the limit close is canceled, got %+v", residual)
	}
}
