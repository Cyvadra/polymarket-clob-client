// Package tactics computes execution lifecycle decisions without performing
// CLOB, NATS, or database side effects.
package tactics

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
)

const defaultTick = 0.01

type Action string

const (
	ActionWait         Action = "WAIT"
	ActionSubmitChild  Action = "SUBMIT_CHILD"
	ActionCancelActive Action = "CANCEL_ACTIVE"
	ActionFinish       Action = "FINISH"
	ActionFail         Action = "FAIL"
)

type Child struct {
	Sequence      int
	State         statemachine.State
	Price         string
	MatchedShares string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type Request struct {
	Intent       protocol.ExecutionIntent
	Quote        marketquotes.Snapshot
	HasQuote     bool
	ActiveChild  *Child
	Children     []Child
	FilledShares string
	Now          time.Time
	LastActionAt time.Time
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
	remaining, err := remainingShares(intent.TargetShares, request.FilledShares)
	if err != nil {
		return fail("invalid shares")
	}
	if remaining <= 0 {
		return Decision{Action: ActionFinish, Reason: "target shares filled"}
	}
	if policy.MaxReprices > 0 && replacementCount(request.Children) >= policy.MaxReprices && request.ActiveChild == nil {
		return Decision{Action: ActionFinish, Reason: "max reprices reached"}
	}
	if request.ActiveChild != nil {
		if !statemachine.RequiresLockedExposure(request.ActiveChild.State) {
			return Decision{Action: ActionSubmitChild, NextSequence: nextSequence(request.Children), Price: intent.LimitPrice, Shares: formatPrice(remaining), PostOnly: intent.PostOnly, TimeInForce: intent.TimeInForce, Reason: "active child terminal"}
		}
		if !intervalElapsed(policy.RepriceIntervalMillis, request.LastActionAt, request.Now) {
			return Decision{Action: ActionWait, Reason: "reprice interval has not elapsed"}
		}
		price, err := targetPrice(intent, request.Quote, request.HasQuote, request.Now)
		if err != nil {
			return Decision{Action: ActionWait, Reason: err.Error()}
		}
		currentPrice, err := parsePrice(request.ActiveChild.Price)
		if err != nil {
			return fail("invalid active child price")
		}
		if math.Abs(price-currentPrice) >= driftThreshold(policy) {
			return Decision{Action: ActionCancelActive, Price: formatPrice(price), Reason: "target price changed"}
		}
		return Decision{Action: ActionWait, Reason: "active child remains within price band"}
	}
	price, err := targetPrice(intent, request.Quote, request.HasQuote, request.Now)
	if err != nil {
		return Decision{Action: ActionWait, Reason: err.Error()}
	}
	return Decision{Action: ActionSubmitChild, NextSequence: nextSequence(request.Children), Price: formatPrice(price), Shares: formatPrice(remaining), PostOnly: postOnly(intent), TimeInForce: timeInForce(intent, style), Reason: "submit next child"}
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
	offset := optionalFloat(policy.QuoteOffset, 0)
	step := optionalFloat(policy.PriceStep, defaultTick)
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
	step := optionalFloat(policy.PriceStep, defaultTick)
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

func remainingShares(target, filled string) (float64, error) {
	targetShares, err := strconv.ParseFloat(target, 64)
	if err != nil || targetShares <= 0 {
		return 0, fmt.Errorf("invalid target shares")
	}
	if strings.TrimSpace(filled) == "" {
		return targetShares, nil
	}
	filledShares, err := strconv.ParseFloat(filled, 64)
	if err != nil || filledShares < 0 {
		return 0, fmt.Errorf("invalid filled shares")
	}
	remaining := targetShares - filledShares
	if remaining < 0 {
		return 0, nil
	}
	return remaining, nil
}

func replacementCount(children []Child) int {
	if len(children) == 0 {
		return 0
	}
	return len(children) - 1
}

func nextSequence(children []Child) int {
	next := 1
	for _, child := range children {
		if child.Sequence >= next {
			next = child.Sequence + 1
		}
	}
	return next
}

func intervalElapsed(intervalMillis int64, lastActionAt, now time.Time) bool {
	if intervalMillis <= 0 || lastActionAt.IsZero() {
		return true
	}
	return !now.Before(lastActionAt.Add(time.Duration(intervalMillis) * time.Millisecond))
}

func driftThreshold(policy protocol.ExecutionPolicy) float64 {
	if policy.PriceStep != "" {
		return optionalFloat(policy.PriceStep, defaultTick)
	}
	if policy.QuoteOffset != "" {
		return optionalFloat(policy.QuoteOffset, defaultTick)
	}
	return defaultTick
}

func timeInForce(intent protocol.ExecutionIntent, style protocol.ExecutionStyle) protocol.TimeInForce {
	if style == protocol.ExecutionStyleTakerAggressive {
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
	price, err := strconv.ParseFloat(value, 64)
	if err != nil || price <= 0 || price >= 1 {
		return 0, fmt.Errorf("invalid price %q", value)
	}
	return price, nil
}

func optionalFloat(value string, fallback float64) float64 {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func validClamped(price float64) (float64, error) {
	if price <= 0 || price >= 1 || math.IsNaN(price) || math.IsInf(price, 0) {
		return 0, fmt.Errorf("planned price is outside valid range")
	}
	return price, nil
}

func formatPrice(value float64) string {
	return strconv.FormatFloat(math.Round(value*1e6)/1e6, 'f', -1, 64)
}

func fail(reason string) Decision {
	return Decision{Action: ActionFail, Reason: reason}
}
