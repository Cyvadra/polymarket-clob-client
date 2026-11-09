package mapping

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

func ladderChild(sequence int, state statemachine.State, matched, price, average string) LadderChild {
	return LadderChild{
		Order:        store.SignedOrderRecord{IntentID: "i", ChildSequence: sequence, State: state, MatchedShares: matched, Price: price},
		AveragePrice: average,
	}
}

func ladderIntent(status statemachine.State) store.OrderIntentRecord {
	return store.OrderIntentRecord{IntentID: "i", RequestID: "req", UniqueTag: "lane", Strategy: "s", Kind: store.IntentOpen, ConditionID: "c", TokenID: "t", Outcome: "Up", Side: store.SideBuy, Status: status}
}

var ladderNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func TestAggregateOpenResultSumsSharesAndWeightsThePriceByShares(t *testing.T) {
	result, err := AggregateOpenResult(ladderIntent(statemachine.StateCanceled), []LadderChild{
		ladderChild(1, statemachine.StateCanceled, "0", "0.50", ""),
		ladderChild(2, statemachine.StateFilled, "20", "0.55", "0.55"),
		ladderChild(3, statemachine.StateCanceled, "10", "0.60", "0.58"), // part filled, then cancelled
		ladderChild(4, statemachine.StateFilled, "30", "0.60", "0.60"),
	}, "done", ladderNow)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.ResultSucceeded || result.ReasonCode != "" {
		t.Fatalf("result=%+v, want a success with no reason", result)
	}
	if result.FilledShares != 60 {
		t.Fatalf("filled=%v, want 60", result.FilledShares)
	}
	want := (20*0.55 + 10*0.58 + 30*0.60) / 60
	if math.Abs(result.AveragePrice-want) > 1e-12 {
		t.Fatalf("average=%v, want the share-weighted %v", result.AveragePrice, want)
	}
	if result.RequestID != "req" || result.UniqueTag != "lane" || result.Strategy != "s" || result.Side != protocol.SideBuy || result.Reason != "done" || !result.OccurredAt.Equal(ladderNow) {
		t.Fatalf("identity fields wrong: %+v", result)
	}
}

func TestAggregateOpenResultFallsBackToTheRestingPriceForAnUnpricedFill(t *testing.T) {
	result, err := AggregateOpenResult(ladderIntent(statemachine.StateFilled), []LadderChild{
		ladderChild(1, statemachine.StateFilled, "10", "0.50", ""),
		ladderChild(2, statemachine.StateFilled, "10", "0.60", "not a price"),
	}, "", ladderNow)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(result.AveragePrice-0.55) > 1e-12 {
		t.Fatalf("average=%v, want 0.55 from the two resting prices", result.AveragePrice)
	}
}

func TestAggregateOpenResultWithNoFillNamesTheLevelNearestTheLimit(t *testing.T) {
	for name, tc := range map[string]struct {
		status   statemachine.State
		children []LadderChild
		want     string
	}{
		"cancel": {statemachine.StateCanceled, []LadderChild{
			ladderChild(1, statemachine.StateRejected, "0", "0.50", ""), ladderChild(2, statemachine.StateCanceled, "0", "0.51", ""),
		}, protocol.ReasonCanceled},
		"the lead is rejected": {statemachine.StateRejected, []LadderChild{
			ladderChild(1, statemachine.StateCanceled, "0", "0.50", ""), ladderChild(2, statemachine.StateRejected, "0", "0.51", ""),
		}, protocol.ReasonOrderRejected},
		"exchange expiry": {statemachine.StateExpired, []LadderChild{
			ladderChild(1, statemachine.StateCanceled, "0", "0.50", ""), ladderChild(2, statemachine.StateExpired, "0", "0.51", ""),
		}, protocol.ReasonExpired},
		"deadline marker": {store.IntentStatusDeadline, []LadderChild{
			ladderChild(1, statemachine.StateCanceled, "0", "0.50", ""), ladderChild(2, statemachine.StateCanceled, "0", "0.51", ""),
		}, protocol.ReasonExpired},
		"drift marker": {store.IntentStatusSignalDrift, []LadderChild{
			ladderChild(1, statemachine.StateCanceled, "0", "0.50", ""), ladderChild(2, statemachine.StateCanceled, "0", "0.51", ""),
		}, protocol.ReasonSignalDrift},
		"failed": {statemachine.StateFailed, []LadderChild{
			ladderChild(2, statemachine.StateFailed, "0", "0.51", ""), ladderChild(1, statemachine.StateCanceled, "0", "0.50", ""),
		}, protocol.ReasonExecutionFailed},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := AggregateOpenResult(ladderIntent(tc.status), tc.children, "", ladderNow)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != protocol.ResultFailed || result.ReasonCode != tc.want || result.FilledShares != 0 || result.AveragePrice != 0 {
				t.Fatalf("result=%+v, want a failed %s with nothing filled", result, tc.want)
			}
		})
	}
}

func TestAggregateOpenResultKeepsAFillOverAnyMarker(t *testing.T) {
	result, err := AggregateOpenResult(ladderIntent(store.IntentStatusDeadline), []LadderChild{
		ladderChild(1, statemachine.StateCanceled, "0", "0.50", ""),
		ladderChild(2, statemachine.StateFilled, "5", "0.51", "0.51"),
	}, "", ladderNow)
	if err != nil || result.Status != protocol.ResultSucceeded || result.FilledShares != 5 {
		t.Fatalf("result=%+v err=%v, want the fill to win over the deadline marker", result, err)
	}
}

func TestAggregateOpenResultRefusesWhatItCannotFold(t *testing.T) {
	for name, tc := range map[string]struct {
		intent   store.OrderIntentRecord
		children []LadderChild
		contains string
	}{
		"a resting child":  {ladderIntent(""), []LadderChild{ladderChild(1, statemachine.StateLive, "0", "0.5", "")}, "not terminal"},
		"a partial child":  {ladderIntent(""), []LadderChild{ladderChild(1, statemachine.StatePartiallyFilled, "1", "0.5", "")}, "not terminal"},
		"no children":      {ladderIntent(""), nil, "no child orders"},
		"negative matched": {ladderIntent(""), []LadderChild{ladderChild(1, statemachine.StateFilled, "-1", "0.5", "")}, "invalid matched shares"},
		"garbled matched":  {ladderIntent(""), []LadderChild{ladderChild(1, statemachine.StateFilled, "lots", "0.5", "")}, "invalid matched shares"},
		"no usable price":  {ladderIntent(""), []LadderChild{ladderChild(1, statemachine.StateFilled, "1", "", "")}, "no usable price"},
		"a close intent":   {store.OrderIntentRecord{IntentID: "i", Kind: store.IntentClose}, []LadderChild{ladderChild(1, statemachine.StateFilled, "1", "0.5", "0.5")}, "not an open"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := AggregateOpenResult(tc.intent, tc.children, "", ladderNow)
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("err=%v, want one containing %q", err, tc.contains)
			}
		})
	}
}
