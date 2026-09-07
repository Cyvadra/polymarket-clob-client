package tactics

import (
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
)

func TestPlanMakerPostOnlyBuySubmitsAtBidOffsetBelowMax(t *testing.T) {
	decision := Plan(Request{Intent: tacticIntent(protocol.SideBuy, protocol.ExecutionStyleMakerPostOnly), Quote: tacticQuote(time.Unix(100, 0).UTC()), HasQuote: true})
	if decision.Action != ActionSubmitChild || decision.Price != "0.43" || !decision.PostOnly || decision.NextSequence != 1 {
		t.Fatalf("unexpected decision: %+v", decision)
	}
}

func TestPlanMakerPostOnlyBuyClampsAtMaxPrice(t *testing.T) {
	intent := tacticIntent(protocol.SideBuy, protocol.ExecutionStyleMakerPostOnly)
	intent.Policy.MidPrice = "0.44"
	intent.Policy.MaxPrice = "0.42"
	decision := Plan(Request{Intent: intent, Quote: tacticQuote(time.Unix(100, 0).UTC()), HasQuote: true})
	if decision.Action != ActionSubmitChild || decision.Price != "0.42" {
		t.Fatalf("expected max-price clamp, got %+v", decision)
	}
}

func TestPlanTakerAggressiveBuyUsesAskPlusStepAndFAK(t *testing.T) {
	intent := tacticIntent(protocol.SideBuy, protocol.ExecutionStyleTakerAggressive)
	intent.TimeInForce = protocol.TimeInForceGTC
	decision := Plan(Request{Intent: intent})
	if decision.Action != ActionSubmitChild || decision.Price != "0.45" || decision.TimeInForce != protocol.TimeInForceFAK || decision.PostOnly {
		t.Fatalf("unexpected taker decision: %+v", decision)
	}
}

func TestPlanUsesSignalPriceWithoutQuote(t *testing.T) {
	intent := tacticIntent(protocol.SideBuy, protocol.ExecutionStyleMakerPostOnly)
	intent.Policy.MidPrice = "0.44"
	decision := Plan(Request{Intent: intent})
	if decision.Action != ActionSubmitChild || decision.Price != "0.43" {
		t.Fatalf("expected signal-priced submission without quote, got %+v", decision)
	}
}

func TestPlanRoundsSharesDownToFourDigits(t *testing.T) {
	intent := tacticIntent(protocol.SideBuy, protocol.ExecutionStyleLimit)
	intent.TargetUSD = "1"
	intent.LimitPrice = "0.43"
	decision := Plan(Request{Intent: intent})
	if decision.Action != ActionSubmitChild || decision.Price != "0.43" || decision.Shares != "2.3255" {
		t.Fatalf("expected rounded shares, got %+v", decision)
	}
}

func TestPlanRejectsAmountBelowSharePrecision(t *testing.T) {
	intent := tacticIntent(protocol.SideBuy, protocol.ExecutionStyleLimit)
	intent.TargetUSD = "0.00001"
	intent.LimitPrice = "0.99"
	decision := Plan(Request{Intent: intent})
	if decision.Action != ActionFail || decision.Reason != "invalid target usd" {
		t.Fatalf("expected invalid target usd, got %+v", decision)
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
	return protocol.ExecutionIntent{SchemaVersion: protocol.SchemaVersionV1, IntentID: "intent-1", ConditionID: "condition", TokenID: "up-token", Outcome: "Up", Side: side, TargetUSD: "0.82", LimitPrice: "0.41", TimeInForce: protocol.TimeInForceGTC, Policy: protocol.ExecutionPolicy{Style: style, MidPrice: "0.44", InitialPrice: "0.41", MaxPrice: maxPrice, MinPrice: minPrice, PriceStep: "0.01", QuoteOffset: "0.01", QuoteMaxAgeMillis: 500}}
}

func tacticQuote(at time.Time) marketquotes.Snapshot {
	return marketquotes.Snapshot{ConditionID: "condition", At: at, Up: marketquotes.Quote{AssetID: "up-token", Bid: 0.42, Ask: 0.46, Mid: 0.44, Timestamp: at}, Down: marketquotes.Quote{AssetID: "down-token", Bid: 0.52, Ask: 0.56, Mid: 0.54, Timestamp: at}}
}
