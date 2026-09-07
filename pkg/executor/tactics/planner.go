// Package tactics computes execution lifecycle decisions without performing
// CLOB, NATS, or database side effects.
package tactics

import (
	"fmt"
	"strings"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
)

// Market carries the exchange rules a plan must respect. Signing rejects any
// price off the tick grid, so the planner needs the grid before it prices.
type Market struct {
	TickSize     float64
	MinOrderSize float64
}

type Request struct {
	Intent protocol.ExecutionIntent
	Market Market
	// AvailableShares is the sellable position for the intent's token. It sizes
	// SELL children and is ignored for BUY.
	AvailableShares string
	Quote           marketquotes.Snapshot
	HasQuote        bool
	Now             time.Time
}

type Decision struct {
	Price       string
	Shares      string
	PostOnly    bool
	TimeInForce protocol.TimeInForce
	Reason      string
}

// Plan produces the single child order for an intent. Any condition that
// prevents planning is an error; there is no deferred or partial decision.
func Plan(request Request) (Decision, error) {
	intent := request.Intent
	if request.Market.TickSize <= 0 {
		return Decision{}, fmt.Errorf("market tick size is required to plan an order")
	}
	policy, err := parsePolicyPrices(intent)
	if err != nil {
		return Decision{}, err
	}
	timeInForce := timeInForce(intent, policy.style)
	price, err := targetPrice(intent, policy, request)
	if err != nil {
		return Decision{}, err
	}
	shares, err := plannedShares(request, price, timeInForce)
	if err != nil {
		return Decision{}, err
	}
	if err := checkMinOrderSize(shares, request.Market.MinOrderSize); err != nil {
		return Decision{}, err
	}
	return Decision{
		Price:       decimal.FormatPrice(price),
		Shares:      shares,
		PostOnly:    intent.PostOnly || policy.style == protocol.ExecutionStyleMakerPostOnly,
		TimeInForce: timeInForce,
		Reason:      "submit initial child",
	}, nil
}

// plannedShares sizes a BUY from target notional and a SELL from shares,
// flooring to the precision the signer encodes so the planned size is the size
// that actually gets signed.
func plannedShares(request Request, price float64, timeInForce protocol.TimeInForce) (string, error) {
	intent := request.Intent
	digits := clobclient.SharePrecisionDigits(intent.Side, timeInForce)
	if intent.Side == protocol.SideSell && intent.Kind == protocol.IntentClose {
		shares, ok := decimal.FloorTo(request.AvailableShares, digits)
		if !ok {
			return "", fmt.Errorf("available position %q rounds to zero at %d decimal places", request.AvailableShares, digits)
		}
		return shares, nil
	}
	shares, ok := decimal.DivideAndRoundDown(intent.TargetUSD, decimal.FormatPrice(price), digits)
	if !ok {
		return "", fmt.Errorf("target usd %q buys less than one unit of share precision at price %s", intent.TargetUSD, decimal.FormatPrice(price))
	}
	return shares, nil
}

func checkMinOrderSize(shares string, minOrderSize float64) error {
	if minOrderSize <= 0 {
		return nil
	}
	parsed, err := decimal.PositiveFloat(shares)
	if err != nil {
		return err
	}
	if parsed+1e-9 < minOrderSize {
		return fmt.Errorf("planned shares %s are below the market minimum order size %.4f", shares, minOrderSize)
	}
	return nil
}

func targetPrice(intent protocol.ExecutionIntent, policy policyPrices, request Request) (float64, error) {
	switch policy.style {
	case protocol.ExecutionStyleLimit:
		return clamp(intent, policy, policy.limit, request.Market.TickSize)
	case protocol.ExecutionStyleTakerAggressive:
		return clamp(intent, policy, takerPrice(intent, policy), request.Market.TickSize)
	default:
		return clamp(intent, policy, makerPrice(intent, policy, request), request.Market.TickSize)
	}
}

func makerPrice(intent protocol.ExecutionIntent, policy policyPrices, request Request) float64 {
	quote, hasQuote := freshQuoteForToken(request)
	step := policy.priceStep
	if step <= 0 {
		step = request.Market.TickSize
	}
	if intent.Side == protocol.SideBuy {
		price := policy.signal - policy.quoteOffset
		if hasQuote && price >= quote.Ask {
			price = quote.Ask - step
		}
		return price
	}
	price := policy.signal + policy.quoteOffset
	if hasQuote && price <= quote.Bid {
		price = quote.Bid + step
	}
	return price
}

func takerPrice(intent protocol.ExecutionIntent, policy policyPrices) float64 {
	if intent.Side == protocol.SideBuy {
		return policy.signal + policy.quoteOffset
	}
	return policy.signal - policy.quoteOffset
}

// clamp bounds the price by the policy limit and snaps it onto the tick grid,
// always toward the side that costs less: down for a buy, up for a sell.
func clamp(intent protocol.ExecutionIntent, policy policyPrices, price float64, tick float64) (float64, error) {
	if intent.Side == protocol.SideBuy {
		if policy.hasMaxPrice && price > policy.maxPrice {
			price = policy.maxPrice
		}
		price = decimal.FloorToTick(price, tick)
	} else {
		if policy.hasMinPrice && price < policy.minPrice {
			price = policy.minPrice
		}
		price = decimal.CeilToTick(price, tick)
	}
	if price <= 0 || price >= 1 {
		return 0, fmt.Errorf("planned price %s is outside the valid range after tick alignment", decimal.FormatPrice(price))
	}
	if intent.Side == protocol.SideBuy && policy.hasMaxPrice && price > policy.maxPrice {
		return 0, fmt.Errorf("planned price %s exceeds max_price after tick alignment", decimal.FormatPrice(price))
	}
	if intent.Side == protocol.SideSell && policy.hasMinPrice && price < policy.minPrice {
		return 0, fmt.Errorf("planned price %s is below min_price after tick alignment", decimal.FormatPrice(price))
	}
	return price, nil
}

// freshQuoteForToken returns the intent token's side of the snapshot, dropping
// it when it is older than the policy's tolerance.
func freshQuoteForToken(request Request) (marketquotes.Quote, bool) {
	if !request.HasQuote {
		return marketquotes.Quote{}, false
	}
	var quote marketquotes.Quote
	switch strings.TrimSpace(request.Intent.TokenID) {
	case strings.TrimSpace(request.Quote.Up.AssetID):
		quote = request.Quote.Up
	case strings.TrimSpace(request.Quote.Down.AssetID):
		quote = request.Quote.Down
	default:
		return marketquotes.Quote{}, false
	}
	maxAge := request.Intent.Policy.QuoteMaxAgeMillis
	if maxAge <= 0 {
		return quote, true
	}
	observedAt := quote.Timestamp
	if observedAt.IsZero() {
		observedAt = request.Quote.At
	}
	if observedAt.IsZero() || request.Now.IsZero() || request.Now.Sub(observedAt) > time.Duration(maxAge)*time.Millisecond {
		return marketquotes.Quote{}, false
	}
	return quote, true
}

func timeInForce(intent protocol.ExecutionIntent, style protocol.ExecutionStyle) protocol.TimeInForce {
	if style == protocol.ExecutionStyleTakerAggressive && (intent.TimeInForce == protocol.TimeInForceGTC || intent.TimeInForce == "") {
		return protocol.TimeInForceFAK
	}
	return intent.TimeInForce
}
