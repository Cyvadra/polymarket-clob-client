package executor

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

func validateIntent(intent protocol.ExecutionIntent) error {
	return validateIntentAt(intent, time.Now().UTC())
}

func validateIntentAt(intent protocol.ExecutionIntent, now time.Time) error {
	if intent.SchemaVersion == "" {
		return fmt.Errorf("execution intent schema_version is required")
	}
	if intent.SchemaVersion != protocol.SchemaVersionV1 {
		return fmt.Errorf("unsupported intent schema version %q", intent.SchemaVersion)
	}
	if intent.IntentID == "" || intent.IdempotencyKey == "" || intent.Strategy == "" || intent.ConditionID == "" || intent.TokenID == "" || intent.Outcome == "" {
		return fmt.Errorf("intent ID, idempotency key, strategy, condition ID, token ID, and outcome are required")
	}
	if intent.Side != protocol.SideBuy && intent.Side != protocol.SideSell {
		return fmt.Errorf("invalid side %q", intent.Side)
	}
	shares, err := decimal.PositiveFloat(intent.TargetShares)
	if err != nil || shares <= 0 {
		return fmt.Errorf("invalid target shares: %w", err)
	}
	price, err := decimal.Price(intent.LimitPrice)
	if err != nil {
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
	if intent.Policy.CompleteWithinMillis < 0 || intent.Policy.CancelTimeoutMillis < 0 || intent.Policy.MaxFeatureAgeMillis < 0 || intent.Policy.RepriceIntervalMillis < 0 || intent.Policy.QuoteMaxAgeMillis < 0 || intent.Policy.SoftCloseAfterMillis < 0 || intent.Policy.ForceCloseAfterMillis < 0 || intent.Policy.CancelReplaceTimeoutMillis < 0 {
		return fmt.Errorf("execution policy durations must not be negative")
	}
	if intent.Policy.MaxReprices < 0 {
		return fmt.Errorf("execution policy max_reprices must not be negative")
	}
	if intent.Policy.RepriceIntervalMillis > 0 || intent.Policy.MaxReprices > 0 || intent.Policy.PostOnlyCrossRetry || intent.Policy.SoftCloseAfterMillis > 0 || intent.Policy.ForceCloseAfterMillis > 0 || intent.Policy.CancelReplaceTimeoutMillis > 0 {
		return fmt.Errorf("execution policy lifecycle controls are not implemented")
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
	if err := validatePolicyTactics(intent, price); err != nil {
		return err
	}
	return nil
}

func validatePolicyTactics(intent protocol.ExecutionIntent, limitPrice float64) error {
	policy := intent.Policy
	switch policy.Style {
	case "", protocol.ExecutionStyleLimit:
		return nil
	case protocol.ExecutionStyleMakerPostOnly, protocol.ExecutionStyleTakerAggressive, protocol.ExecutionStyleAuto:
	default:
		return fmt.Errorf("unsupported execution style %q", intent.Policy.Style)
	}
	initialPrice, err := optionalPolicyPrice("initial_price", policy.InitialPrice, limitPrice)
	if err != nil {
		return err
	}
	maxPrice, hasMaxPrice, err := optionalPolicyBound("max_price", policy.MaxPrice)
	if err != nil {
		return err
	}
	minPrice, hasMinPrice, err := optionalPolicyBound("min_price", policy.MinPrice)
	if err != nil {
		return err
	}
	if policy.PriceStep != "" {
		if step, err := decimal.Price(policy.PriceStep); err != nil || step <= 0 || step >= 1 {
			return fmt.Errorf("invalid execution policy price_step %q", policy.PriceStep)
		}
	}
	if policy.QuoteOffset != "" {
		if offset, err := decimal.NonNegativeFloat(policy.QuoteOffset); err != nil || offset >= 1 {
			return fmt.Errorf("invalid execution policy quote_offset %q", policy.QuoteOffset)
		}
	}
	if intent.Side == protocol.SideBuy {
		if !hasMaxPrice {
			return fmt.Errorf("buy execution policy requires max_price")
		}
		if initialPrice > maxPrice {
			return fmt.Errorf("buy execution policy initial_price exceeds max_price")
		}
	}
	if intent.Side == protocol.SideSell {
		if !hasMinPrice {
			return fmt.Errorf("sell execution policy requires min_price")
		}
		if initialPrice < minPrice {
			return fmt.Errorf("sell execution policy initial_price is below min_price")
		}
	}
	return nil
}

func optionalPolicyPrice(name, value string, fallback float64) (float64, error) {
	if value == "" {
		return fallback, nil
	}
	price, err := decimal.Price(value)
	if err != nil {
		return 0, fmt.Errorf("invalid execution policy %s %q", name, value)
	}
	return price, nil
}

func optionalPolicyBound(name, value string) (float64, bool, error) {
	if value == "" {
		return 0, false, nil
	}
	price, err := optionalPolicyPrice(name, value, 0)
	if err != nil {
		return 0, false, err
	}
	return price, true, nil
}

func validationReasonCode(err error) string {
	if strings.Contains(err.Error(), "unsupported execution style") {
		return "UNSUPPORTED_EXECUTION_STYLE"
	}
	if strings.Contains(err.Error(), "not implemented") {
		return "UNIMPLEMENTED_POLICY"
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

func executionIntent(record store.OrderIntentRecord) protocol.ExecutionIntent {
	return protocol.ExecutionIntent{
		SchemaVersion: protocol.SchemaVersionV1, IntentID: record.IntentID, IdempotencyKey: record.IdempotencyKey, Strategy: record.Strategy,
		Kind: protocol.IntentKind(record.Kind), MarketID: record.MarketID, EventSlug: record.EventSlug,
		ConditionID: record.ConditionID, TokenID: record.TokenID, Outcome: record.Outcome, Side: record.Side,
		TargetShares: record.TargetShares, LimitPrice: record.LimitPrice, TimeInForce: record.TimeInForce,
		PostOnly: record.PostOnly, FeatureSeq: record.FeatureSeq, FeatureCompletedAt: record.FeatureCompletedAt,
		ExpiresAt: record.ExpiresAt, Policy: record.Policy,
	}
}

func sameIntent(intent protocol.ExecutionIntent, record store.OrderIntentRecord) bool {
	return intent.IntentID == record.IntentID && intent.IdempotencyKey == record.IdempotencyKey && intent.Strategy == record.Strategy &&
		intent.Kind == record.Kind && intent.MarketID == record.MarketID && intent.EventSlug == record.EventSlug &&
		intent.ConditionID == record.ConditionID && intent.TokenID == record.TokenID && intent.Outcome == record.Outcome &&
		intent.Side == record.Side && sameDecimal(intent.TargetShares, record.TargetShares) && sameDecimal(intent.LimitPrice, record.LimitPrice) &&
		intent.TimeInForce == record.TimeInForce && intent.PostOnly == record.PostOnly && intent.FeatureSeq == record.FeatureSeq &&
		intent.FeatureCompletedAt.Equal(record.FeatureCompletedAt) && intent.ExpiresAt.Equal(record.ExpiresAt) && intent.Policy == record.Policy
}

func sameDecimal(left, right string) bool {
	leftRat, leftOK := decimal.Rat(left)
	rightRat, rightOK := decimal.Rat(right)
	if !leftOK || !rightOK {
		return strings.TrimSpace(left) == strings.TrimSpace(right)
	}
	return leftRat.Cmp(rightRat) == 0
}

func reservationRecord(intent protocol.ExecutionIntent, child plannedChild, reservationID string, now time.Time) store.ReservationRecord {
	notional := "0"
	if product, ok := decimal.MulString(child.Shares, child.Price); ok {
		notional = product
	}
	return store.ReservationRecord{ReservationID: reservationID, IntentID: intent.IntentID, ChildSequence: child.Sequence, MarketID: intent.MarketID, ConditionID: intent.ConditionID, TokenID: intent.TokenID, Outcome: intent.Outcome, Side: intent.Side, Shares: child.Shares, Notional: notional, State: "active", Reason: "execution intent accepted", CreatedAt: now, UpdatedAt: now}
}

func reservationID(intentID string, childSequence int) string {
	return fmt.Sprintf("%s:%d", intentID, childSequence)
}

func userOrder(intent protocol.ExecutionIntent, child plannedChild) (clobclient.UserOrder, error) {
	shares, err := strconv.ParseFloat(child.Shares, 64)
	if err != nil || shares <= 0 {
		return clobclient.UserOrder{}, fmt.Errorf("invalid planned shares %q", child.Shares)
	}
	price, err := strconv.ParseFloat(child.Price, 64)
	if err != nil || price <= 0 || price >= 1 {
		return clobclient.UserOrder{}, fmt.Errorf("invalid planned price %q", child.Price)
	}
	return clobclient.UserOrder{TokenID: intent.TokenID, Side: intent.Side, Shares: shares, Price: price, PostOnly: child.PostOnly, OrderType: child.TimeInForce}, nil
}
