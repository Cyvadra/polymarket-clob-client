package tactics

import (
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
)

var quoteTime = time.Unix(100, 0).UTC()

func TestPlanMakerPostOnlyBuySubmitsAtBidOffsetBelowMax(t *testing.T) {
	decision := mustPlan(t, buyRequest(protocol.ExecutionStyleMakerPostOnly))
	if decision.Price != "0.43" || !decision.PostOnly {
		t.Fatalf("unexpected decision: %+v", decision)
	}
}

// A maker only ever prices inward from the signal, so the bound bites on the
// taker path, where the offset pushes the price outward.
func TestPlanTakerBuyClampsAtMaxPrice(t *testing.T) {
	request := buyRequest(protocol.ExecutionStyleTakerAggressive)
	request.HasQuote = false
	request.Intent.Policy.MidPrice = "0.44"
	request.Intent.Policy.QuoteOffset = "0.05"
	request.Intent.Policy.MaxPrice = "0.46"
	if decision := mustPlan(t, request); decision.Price != "0.46" {
		t.Fatalf("expected max-price clamp, got %+v", decision)
	}
}

func TestPlanTakerSellClampsAtMinPrice(t *testing.T) {
	request := sellRequest(protocol.ExecutionStyleTakerAggressive, "10")
	request.HasQuote = false
	request.Intent.Policy.MidPrice = "0.44"
	request.Intent.Policy.QuoteOffset = "0.05"
	request.Intent.Policy.MinPrice = "0.41"
	if decision := mustPlan(t, request); decision.Price != "0.41" {
		t.Fatalf("expected min-price clamp, got %+v", decision)
	}
}

func TestPlanTakerAggressiveBuyUsesOffsetAndFAK(t *testing.T) {
	request := buyRequest(protocol.ExecutionStyleTakerAggressive)
	request.HasQuote = false
	request.Intent.TimeInForce = protocol.TimeInForceGTC
	decision := mustPlan(t, request)
	if decision.Price != "0.45" || decision.TimeInForce != protocol.TimeInForceFAK || decision.PostOnly {
		t.Fatalf("unexpected taker decision: %+v", decision)
	}
}

func TestPlanUsesSignalPriceWithoutQuote(t *testing.T) {
	request := buyRequest(protocol.ExecutionStyleMakerPostOnly)
	request.HasQuote = false
	request.Intent.Policy.MidPrice = "0.44"
	if decision := mustPlan(t, request); decision.Price != "0.43" {
		t.Fatalf("expected signal-priced submission without quote, got %+v", decision)
	}
}

// A buy that cannot rest is sized to four decimals; a resting buy to two.
func TestPlanSizesBuySharesAtTheSignedPrecision(t *testing.T) {
	request := buyRequest(protocol.ExecutionStyleLimit)
	request.Intent.TargetUSD = "1"
	request.Intent.LimitPrice = "0.43"
	request.Intent.TimeInForce = protocol.TimeInForceFAK
	if decision := mustPlan(t, request); decision.Shares != "2.3255" {
		t.Fatalf("expected four-decimal shares for an immediate buy, got %+v", decision)
	}

	request.Intent.TimeInForce = protocol.TimeInForceGTC
	if decision := mustPlan(t, request); decision.Shares != "2.32" {
		t.Fatalf("expected two-decimal shares for a resting buy, got %+v", decision)
	}
}

func TestPlanRejectsAmountBelowSharePrecision(t *testing.T) {
	request := buyRequest(protocol.ExecutionStyleLimit)
	request.Intent.TargetUSD = "0.00001"
	request.Intent.LimitPrice = "0.99"
	if _, err := Plan(request); err == nil {
		t.Fatal("expected a target below share precision to be unplannable")
	}
}

// Signing rejects any price off the tick grid, so the planner must land on it.
func TestPlanSnapsOffTickSignalPriceToThePassiveSide(t *testing.T) {
	request := buyRequest(protocol.ExecutionStyleTakerAggressive)
	request.HasQuote = false
	request.Intent.Policy.MidPrice = "0.545"
	request.Intent.Policy.QuoteOffset = ""
	request.Intent.Policy.MaxPrice = "0.60"
	if decision := mustPlan(t, request); decision.Price != "0.54" {
		t.Fatalf("expected a buy to snap down onto the tick grid, got %+v", decision)
	}

	sell := sellRequest(protocol.ExecutionStyleTakerAggressive, "10")
	sell.HasQuote = false
	sell.Intent.Policy.MidPrice = "0.545"
	sell.Intent.Policy.QuoteOffset = ""
	sell.Intent.Policy.MinPrice = "0.40"
	if decision := mustPlan(t, sell); decision.Price != "0.55" {
		t.Fatalf("expected a sell to snap up onto the tick grid, got %+v", decision)
	}
}

func TestPlanKeepsPriceAlreadyOnTheTickGrid(t *testing.T) {
	request := buyRequest(protocol.ExecutionStyleLimit)
	request.Intent.LimitPrice = "0.43"
	if decision := mustPlan(t, request); decision.Price != "0.43" {
		t.Fatalf("expected an on-grid price to survive alignment, got %+v", decision)
	}
}

func TestPlanIgnoresQuoteOlderThanPolicyMaxAge(t *testing.T) {
	request := buyRequest(protocol.ExecutionStyleMakerPostOnly)
	request.Intent.Policy.MidPrice = "0.44"
	request.Intent.Policy.QuoteMaxAgeMillis = 500
	// The quote would price this at 0.43; stale, it falls back to the signal.
	request.Now = quoteTime.Add(2 * time.Second)
	if decision := mustPlan(t, request); decision.Price != "0.43" {
		t.Fatalf("expected the stale quote to be ignored, got %+v", decision)
	}

	fresh := buyRequest(protocol.ExecutionStyleMakerPostOnly)
	fresh.Intent.Policy.MidPrice = "0.50"
	fresh.Intent.Policy.QuoteMaxAgeMillis = 500
	fresh.Now = quoteTime.Add(100 * time.Millisecond)
	if decision := mustPlan(t, fresh); decision.Price != "0.45" {
		t.Fatalf("expected a fresh quote to cap the price below the ask, got %+v", decision)
	}

	stale := fresh
	stale.Now = quoteTime.Add(2 * time.Second)
	if decision := mustPlan(t, stale); decision.Price != "0.49" {
		t.Fatalf("expected a stale quote to leave the signal price alone, got %+v", decision)
	}
}

func TestPlanCloseUsesActualPositionShares(t *testing.T) {
	request := sellRequest(protocol.ExecutionStyleLimit, "12.3456")
	request.Intent.Kind = protocol.IntentClose
	if decision := mustPlan(t, request); decision.Shares != "12.34" {
		t.Fatalf("expected actual position shares, got %+v", decision)
	}
}

func TestPlanSellUsesTargetUSD(t *testing.T) {
	request := sellRequest(protocol.ExecutionStyleLimit, "12.3456")
	if decision := mustPlan(t, request); decision.Shares != "2.00" {
		t.Fatalf("expected target USD to size a non-close sell, got %+v", decision)
	}
}

func TestPlanCloseNeverExceedsTheAvailablePosition(t *testing.T) {
	request := sellRequest(protocol.ExecutionStyleLimit, "3")
	request.Intent.Kind = protocol.IntentClose
	if decision := mustPlan(t, request); decision.Shares != "3.00" {
		t.Fatalf("expected the sell to be capped at the holding, got %+v", decision)
	}
}

func TestPlanRejectsCloseWithNoPosition(t *testing.T) {
	request := sellRequest(protocol.ExecutionStyleLimit, "0")
	request.Intent.Kind = protocol.IntentClose
	if _, err := Plan(request); err == nil {
		t.Fatal("expected a sell with no position to be unplannable")
	}
}

func TestPlanRejectsSharesBelowMinimumOrderSize(t *testing.T) {
	request := sellRequest(protocol.ExecutionStyleLimit, "1")
	request.Market.MinOrderSize = 5
	if _, err := Plan(request); err == nil {
		t.Fatal("expected shares below the market minimum to be unplannable")
	}
}

func TestPlanRequiresTickSize(t *testing.T) {
	request := buyRequest(protocol.ExecutionStyleLimit)
	request.Market.TickSize = 0
	if _, err := Plan(request); err == nil {
		t.Fatal("expected planning without a tick size to fail")
	}
}

func mustPlan(t *testing.T, request Request) Decision {
	t.Helper()
	decision, err := Plan(request)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return decision
}

func buyRequest(style protocol.ExecutionStyle) Request {
	intent := protocol.ExecutionIntent{
		SchemaVersion: protocol.SchemaVersionV1, IntentID: "intent-1", ConditionID: "condition", TokenID: "up-token",
		Outcome: "Up", Side: protocol.SideBuy, TargetUSD: "0.82", LimitPrice: "0.41", TimeInForce: protocol.TimeInForceGTC,
		Policy: protocol.ExecutionPolicy{Style: style, MidPrice: "0.44", InitialPrice: "0.41", MaxPrice: "0.55", PriceStep: "0.01", QuoteOffset: "0.01"},
	}
	return Request{Intent: intent, Market: Market{TickSize: 0.01}, Quote: tacticQuote(), HasQuote: true, Now: quoteTime}
}

func sellRequest(style protocol.ExecutionStyle, available string) Request {
	intent := protocol.ExecutionIntent{
		SchemaVersion: protocol.SchemaVersionV1, IntentID: "intent-1", ConditionID: "condition", TokenID: "up-token",
		Outcome: "Up", Side: protocol.SideSell, TargetUSD: "0.82", LimitPrice: "0.41", TimeInForce: protocol.TimeInForceGTC,
		Policy: protocol.ExecutionPolicy{Style: style, MidPrice: "0.44", InitialPrice: "0.41", MinPrice: "0.35", PriceStep: "0.01", QuoteOffset: "0.01"},
	}
	return Request{Intent: intent, Market: Market{TickSize: 0.01}, AvailableShares: available, Quote: tacticQuote(), HasQuote: true, Now: quoteTime}
}

func tacticQuote() marketquotes.Snapshot {
	return marketquotes.Snapshot{
		ConditionID: "condition", At: quoteTime,
		Up:   marketquotes.Quote{AssetID: "up-token", Bid: 0.42, Ask: 0.46, Mid: 0.44, Timestamp: quoteTime},
		Down: marketquotes.Quote{AssetID: "down-token", Bid: 0.52, Ask: 0.56, Mid: 0.54, Timestamp: quoteTime},
	}
}
