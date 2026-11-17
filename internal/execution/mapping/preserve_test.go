package mapping

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

func preserveRecord(t *testing.T, requested, shares string) store.OrderIntentRecord {
	t.Helper()
	policy, err := json.Marshal(protocol.ExecutionPolicy{Style: protocol.ExecutionStyleSplitPreserve, Preserve: &protocol.PreserveLeg{
		UniqueTag: "lane", TokenID: "up", Outcome: "Up", LimitPrice: "0.6", RequestedShares: requested, Shares: shares,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return store.OrderIntentRecord{
		IntentID: "intent", RequestID: "req", UniqueTag: "lane|inv", Strategy: "s", Kind: store.IntentOpen,
		ConditionID: "c", TokenID: "down", Outcome: "Down", Side: store.SideSell, Policy: policy,
	}
}

func TestTerminalResultReportsAPreserveAsTheBuy(t *testing.T) {
	order := store.SignedOrderRecord{ChildSequence: store.StrategyChildSequence, State: statemachine.StateFilled, MatchedShares: "50"}
	result, ok := TerminalResult(order, preserveRecord(t, "50", "50"), "", "0.4", time.Unix(1, 0))
	if !ok {
		t.Fatal("no result")
	}
	if result.UniqueTag != "lane" || result.TokenID != "up" || result.Outcome != "Up" || result.Side != protocol.SideBuy {
		t.Fatalf("result names the sell, not the buy: %+v", result)
	}
	if result.FilledShares != 50 || math.Abs(result.AveragePrice-0.6) > 1e-9 || result.ReasonCode != "" {
		t.Fatalf("unexpected fill: %+v", result)
	}
}

func TestTerminalResultMarksACappedPreserve(t *testing.T) {
	order := store.SignedOrderRecord{ChildSequence: store.StrategyChildSequence, State: statemachine.StateFilled, MatchedShares: "100"}
	result, _ := TerminalResult(order, preserveRecord(t, "500", "100"), "", "0.9", time.Unix(1, 0))
	if result.Status != protocol.ResultSucceeded || result.ReasonCode != protocol.ReasonInventoryCapped {
		t.Fatalf("capped preserve: %+v", result)
	}
}

func TestPreserveOpenResultLeavesOtherOpensAlone(t *testing.T) {
	in := protocol.ExecutionOpenResult{TokenID: "t", Side: protocol.SideBuy, AveragePrice: 0.3}
	if out := PreserveOpenResult(in, protocol.ExecutionPolicy{Style: protocol.ExecutionStyleLimit}); out != in {
		t.Fatalf("changed a plain open: %+v", out)
	}
}
