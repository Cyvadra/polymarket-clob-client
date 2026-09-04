package executor

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

func validateIntent(intent protocol.ExecutionIntent) error {
	return validateIntentAt(intent, time.Now().UTC())
}

func validateIntentAt(intent protocol.ExecutionIntent, now time.Time) error {
	if intent.SchemaVersion != "" && intent.SchemaVersion != protocol.SchemaVersionV1 {
		return fmt.Errorf("unsupported intent schema version %q", intent.SchemaVersion)
	}
	if intent.IntentID == "" || intent.IdempotencyKey == "" || intent.Strategy == "" || intent.ConditionID == "" || intent.TokenID == "" || intent.Outcome == "" {
		return fmt.Errorf("intent ID, idempotency key, strategy, condition ID, token ID, and outcome are required")
	}
	if intent.Side != protocol.SideBuy && intent.Side != protocol.SideSell {
		return fmt.Errorf("invalid side %q", intent.Side)
	}
	shares, err := strconv.ParseFloat(intent.TargetShares, 64)
	if err != nil || shares <= 0 {
		return fmt.Errorf("invalid target shares: %w", err)
	}
	price, err := strconv.ParseFloat(intent.LimitPrice, 64)
	if err != nil || price <= 0 || price >= 1 {
		return fmt.Errorf("invalid limit price %q", intent.LimitPrice)
	}
	if intent.TimeInForce != protocol.TimeInForceGTC && intent.TimeInForce != protocol.TimeInForceFOK && intent.TimeInForce != protocol.TimeInForceFAK && intent.TimeInForce != protocol.TimeInForceGTD {
		return fmt.Errorf("invalid time in force %q", intent.TimeInForce)
	}
	if !intent.ExpiresAt.IsZero() && !intent.ExpiresAt.After(now) {
		return fmt.Errorf("execution intent expired at %s", intent.ExpiresAt.UTC().Format(time.RFC3339Nano))
	}
	if intent.ExpiresAt.IsZero() && intent.Policy.CompleteWithinMillis == 0 {
		return fmt.Errorf("execution intent requires expires_at or complete_within_ms")
	}
	if intent.Policy.CompleteWithinMillis < 0 || intent.Policy.CancelTimeoutMillis < 0 || intent.Policy.MaxFeatureAgeMillis < 0 {
		return fmt.Errorf("execution policy durations must not be negative")
	}
	if intent.Kind != protocol.IntentOpen && intent.Kind != protocol.IntentClose {
		return fmt.Errorf("invalid intent kind %q", intent.Kind)
	}
	if intent.Kind == protocol.IntentClose && intent.Side != protocol.SideSell {
		return fmt.Errorf("close execution intent must sell")
	}
	if intent.Policy.MaxFeatureAgeMillis > 0 && (intent.FeatureCompletedAt.IsZero() || now.Sub(intent.FeatureCompletedAt) > time.Duration(intent.Policy.MaxFeatureAgeMillis)*time.Millisecond) {
		return fmt.Errorf("execution intent feature is stale")
	}
	if intent.Policy.Style != "" && intent.Policy.Style != protocol.ExecutionStyleLimit {
		return fmt.Errorf("unsupported execution style %q", intent.Policy.Style)
	}
	return nil
}

func validationReasonCode(err error) string {
	if strings.Contains(err.Error(), "unsupported execution style") {
		return "UNSUPPORTED_EXECUTION_STYLE"
	}
	return "INVALID_INTENT"
}

func intentRecord(intent protocol.ExecutionIntent, now time.Time) store.OrderIntentRecord {
	return store.OrderIntentRecord{
		IntentID: intent.IntentID, IdempotencyKey: intent.IdempotencyKey, Strategy: intent.Strategy, MarketID: intent.MarketID,
		Kind: intent.Kind, EventSlug: intent.EventSlug, ConditionID: intent.ConditionID, TokenID: intent.TokenID, Outcome: intent.Outcome,
		Side: intent.Side, TargetShares: intent.TargetShares, LimitPrice: intent.LimitPrice, TimeInForce: intent.TimeInForce,
		PostOnly: intent.PostOnly, FeatureSeq: intent.FeatureSeq, FeatureCompletedAt: intent.FeatureCompletedAt, ExpiresAt: intent.ExpiresAt,
		Status: statemachine.StateIntentReceived, Policy: intent.Policy, CreatedAt: now, UpdatedAt: now,
	}
}

func reservationRecord(intent protocol.ExecutionIntent, reservationID string, now time.Time) store.ReservationRecord {
	notional := "0"
	if shares, sharesOK := newRat(intent.TargetShares); sharesOK {
		if price, priceOK := newRat(intent.LimitPrice); priceOK {
			notional = new(big.Rat).Mul(shares, price).FloatString(18)
		}
	}
	return store.ReservationRecord{ReservationID: reservationID, IntentID: intent.IntentID, ChildSequence: 1, MarketID: intent.MarketID, ConditionID: intent.ConditionID, TokenID: intent.TokenID, Outcome: intent.Outcome, Side: intent.Side, Shares: intent.TargetShares, Notional: notional, State: "active", Reason: "execution intent accepted", CreatedAt: now, UpdatedAt: now}
}

func reservationID(intentID string, childSequence int) string {
	return fmt.Sprintf("%s:%d", intentID, childSequence)
}

func newRat(value string) (*big.Rat, bool) {
	return new(big.Rat).SetString(value)
}

func userOrder(intent protocol.ExecutionIntent) (clobclient.UserOrder, error) {
	shares, err := strconv.ParseFloat(intent.TargetShares, 64)
	if err != nil || shares <= 0 {
		return clobclient.UserOrder{}, fmt.Errorf("invalid target shares %q", intent.TargetShares)
	}
	price, err := strconv.ParseFloat(intent.LimitPrice, 64)
	if err != nil || price <= 0 || price >= 1 {
		return clobclient.UserOrder{}, fmt.Errorf("invalid limit price %q", intent.LimitPrice)
	}
	return clobclient.UserOrder{TokenID: intent.TokenID, Side: intent.Side, Shares: shares, Price: price, PostOnly: intent.PostOnly, OrderType: intent.TimeInForce}, nil
}
