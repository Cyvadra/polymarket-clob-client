package tactics

import (
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
)

func TestPlanMakerPostOnlyBuySubmitsAtBidOffsetBelowMax(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	decision := Plan(Request{Intent: tacticIntent(protocol.SideBuy, protocol.ExecutionStyleMakerPostOnly), Quote: tacticQuote(now), HasQuote: true, Now: now})
	if decision.Action != ActionSubmitChild || decision.Price != "0.43" || !decision.PostOnly || decision.NextSequence != 1 {
		t.Fatalf("unexpected decision: %+v", decision)
	}
}

func TestPlanMakerPostOnlyBuyClampsAtMaxPrice(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	intent := tacticIntent(protocol.SideBuy, protocol.ExecutionStyleMakerPostOnly)
	intent.Policy.MaxPrice = "0.42"
	decision := Plan(Request{Intent: intent, Quote: tacticQuote(now), HasQuote: true, Now: now})
	if decision.Action != ActionSubmitChild || decision.Price != "0.42" {
		t.Fatalf("expected max-price clamp, got %+v", decision)
	}
}

func TestPlanCancelsActiveChildWhenQuoteDrifts(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	child := &Child{Sequence: 1, State: statemachine.StateLive, Price: "0.40"}
	decision := Plan(Request{Intent: tacticIntent(protocol.SideBuy, protocol.ExecutionStyleMakerPostOnly), Quote: tacticQuote(now), HasQuote: true, ActiveChild: child, Now: now, LastActionAt: now.Add(-time.Second)})
	if decision.Action != ActionCancelActive || decision.Price != "0.43" {
		t.Fatalf("expected cancel for repricing, got %+v", decision)
	}
}

func TestPlanWaitsWhenRepriceIntervalHasNotElapsed(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	child := &Child{Sequence: 1, State: statemachine.StateLive, Price: "0.40"}
	decision := Plan(Request{Intent: tacticIntent(protocol.SideBuy, protocol.ExecutionStyleMakerPostOnly), Quote: tacticQuote(now), HasQuote: true, ActiveChild: child, Now: now, LastActionAt: now.Add(-100 * time.Millisecond)})
	if decision.Action != ActionWait {
		t.Fatalf("expected wait, got %+v", decision)
	}
}

func TestPlanTakerAggressiveBuyUsesAskPlusStepAndFAK(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	intent := tacticIntent(protocol.SideBuy, protocol.ExecutionStyleTakerAggressive)
	intent.TimeInForce = protocol.TimeInForceGTC
	decision := Plan(Request{Intent: intent, Quote: tacticQuote(now), HasQuote: true, Now: now})
	if decision.Action != ActionSubmitChild || decision.Price != "0.47" || decision.TimeInForce != protocol.TimeInForceFAK || decision.PostOnly {
		t.Fatalf("unexpected taker decision: %+v", decision)
	}
}

func TestPlanWaitsForFreshQuote(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	quote := tacticQuote(now.Add(-time.Second))
	decision := Plan(Request{Intent: tacticIntent(protocol.SideBuy, protocol.ExecutionStyleMakerPostOnly), Quote: quote, HasQuote: true, Now: now})
	if decision.Action != ActionWait || decision.Reason != "quote is stale" {
		t.Fatalf("expected stale quote wait, got %+v", decision)
	}
}

func TestPlanSubmitsNextChildOnlyAfterTerminalActiveChild(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	child := Child{Sequence: 1, State: statemachine.StateCanceled, Price: "0.40"}
	decision := Plan(Request{Intent: tacticIntent(protocol.SideBuy, protocol.ExecutionStyleMakerPostOnly), Quote: tacticQuote(now), HasQuote: true, Children: []Child{child}, ActiveChild: &child, Now: now})
	if decision.Action != ActionSubmitChild || decision.NextSequence != 2 {
		t.Fatalf("expected next child after terminal active child, got %+v", decision)
	}
}

func tacticIntent(side protocol.Side, style protocol.ExecutionStyle) protocol.ExecutionIntent {
	minPrice := "0.35"
	maxPrice := "0.55"
	if side == protocol.SideSell {
		maxPrice = ""
	} else {
		minPrice = ""
	}
	return protocol.ExecutionIntent{IntentID: "intent-1", ConditionID: "condition", TokenID: "up-token", Outcome: "Up", Side: side, TargetShares: "2", LimitPrice: "0.41", TimeInForce: protocol.TimeInForceGTC, Policy: protocol.ExecutionPolicy{Style: style, InitialPrice: "0.41", MaxPrice: maxPrice, MinPrice: minPrice, PriceStep: "0.01", QuoteOffset: "0.01", RepriceIntervalMillis: 250, MaxReprices: 3, QuoteMaxAgeMillis: 500}}
}

func tacticQuote(at time.Time) marketquotes.Snapshot {
	return marketquotes.Snapshot{ConditionID: "condition", At: at, Up: marketquotes.Quote{AssetID: "up-token", Bid: 0.42, Ask: 0.46, Mid: 0.44, Timestamp: at}, Down: marketquotes.Quote{AssetID: "down-token", Bid: 0.52, Ask: 0.56, Mid: 0.54, Timestamp: at}}
}
