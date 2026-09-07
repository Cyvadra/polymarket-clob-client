// Package tactics computes execution lifecycle decisions without performing
// CLOB, NATS, or database side effects.
package tactics

import (
	"fmt"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
)

const defaultTick = 0.01

type Action string

const (
	ActionWait        Action = "WAIT"
	ActionSubmitChild Action = "SUBMIT_CHILD"
	ActionFail        Action = "FAIL"
)

type Request struct {
	Intent   protocol.ExecutionIntent
	Quote    marketquotes.Snapshot
	HasQuote bool
	Now      time.Time
}

type Decision struct {
	Action       Action
	NextSequence int
	Price        string
	Shares       string
	PostOnly     bool
	TimeInForce  protocol.TimeInForce
	Reason       string
}

func Plan(request Request) Decision {
	intent := request.Intent
	policy := intent.Policy
	style := policy.Style
	if style == "" {
		style = protocol.ExecutionStyleLimit
	}
	price, err := targetPrice(intent, request.Quote, request.HasQuote, request.Now)
	if err != nil {
		return Decision{Action: ActionWait, Reason: err.Error()}
	}
	shares, ok := decimal.DivideAndRoundDown(intent.TargetUSD, decimal.FormatPrice(price), 4)
	if !ok {
		return fail("invalid target usd")
	}
	return Decision{Action: ActionSubmitChild, NextSequence: 1, Price: decimal.FormatPrice(price), Shares: shares, PostOnly: postOnly(intent), TimeInForce: timeInForce(intent, style), Reason: "submit initial child"}
}

func targetPrice(intent protocol.ExecutionIntent, snapshot marketquotes.Snapshot, hasQuote bool, now time.Time) (float64, error) {
	policy := intent.Policy
	style := policy.Style
	if style == "" || style == protocol.ExecutionStyleLimit {
		return parsePrice(intent.LimitPrice)
	}
	if !hasQuote {
		return 0, fmt.Errorf("quote is required")
	}
	if policy.QuoteMaxAgeMillis > 0 && now.Sub(snapshot.At) > time.Duration(policy.QuoteMaxAgeMillis)*time.Millisecond {
		return 0, fmt.Errorf("quote is stale")
	}
	quote, ok := quoteForToken(snapshot, intent.TokenID)
	if !ok {
		return 0, fmt.Errorf("quote token mismatch")
	}
	if style == protocol.ExecutionStyleTakerAggressive || style == protocol.ExecutionStyleAuto {
		return takerPrice(intent, quote)
	}
	return makerPrice(intent, quote)
}

func makerPrice(intent protocol.ExecutionIntent, quote marketquotes.Quote) (float64, error) {
	policy := intent.Policy
	offset := decimal.OptionalFloat(policy.QuoteOffset, 0)
	step := decimal.OptionalFloat(policy.PriceStep, defaultTick)
	if intent.Side == protocol.SideBuy {
		price := quote.Bid + offset
		if price >= quote.Ask {
			price = quote.Ask - step
		}
		return clampBuy(intent, price)
	}
	price := quote.Ask - offset
	if price <= quote.Bid {
		price = quote.Bid + step
	}
	return clampSell(intent, price)
}

func takerPrice(intent protocol.ExecutionIntent, quote marketquotes.Quote) (float64, error) {
	policy := intent.Policy
	step := decimal.OptionalFloat(policy.PriceStep, defaultTick)
	if intent.Side == protocol.SideBuy {
		return clampBuy(intent, quote.Ask+step)
	}
	return clampSell(intent, quote.Bid-step)
}

func clampBuy(intent protocol.ExecutionIntent, price float64) (float64, error) {
	maxPrice, err := requiredPolicyPrice("max_price", intent.Policy.MaxPrice)
	if err != nil {
		return 0, err
	}
	if price > maxPrice {
		price = maxPrice
	}
	return validClamped(price)
}

func clampSell(intent protocol.ExecutionIntent, price float64) (float64, error) {
	minPrice, err := requiredPolicyPrice("min_price", intent.Policy.MinPrice)
	if err != nil {
		return 0, err
	}
	if price < minPrice {
		price = minPrice
	}
	return validClamped(price)
}

func quoteForToken(snapshot marketquotes.Snapshot, tokenID string) (marketquotes.Quote, bool) {
	if strings.TrimSpace(snapshot.Up.AssetID) == strings.TrimSpace(tokenID) {
		return snapshot.Up, true
	}
	if strings.TrimSpace(snapshot.Down.AssetID) == strings.TrimSpace(tokenID) {
		return snapshot.Down, true
	}
	return marketquotes.Quote{}, false
}

func timeInForce(intent protocol.ExecutionIntent, style protocol.ExecutionStyle) protocol.TimeInForce {
	if style == protocol.ExecutionStyleTakerAggressive || style == protocol.ExecutionStyleAuto {
		if intent.TimeInForce == protocol.TimeInForceGTC || intent.TimeInForce == "" {
			return protocol.TimeInForceFAK
		}
	}
	return intent.TimeInForce
}

func postOnly(intent protocol.ExecutionIntent) bool {
	return intent.PostOnly || intent.Policy.Style == protocol.ExecutionStyleMakerPostOnly
}

func requiredPolicyPrice(name, value string) (float64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, fmt.Errorf("%s is required", name)
	}
	return parsePrice(value)
}

func parsePrice(value string) (float64, error) {
	return decimal.Price(value)
}

func validClamped(price float64) (float64, error) {
	if price <= 0 || price >= 1 {
		return 0, fmt.Errorf("planned price is outside valid range")
	}
	return price, nil
}

func fail(reason string) Decision {
	return Decision{Action: ActionFail, Reason: reason}
}
