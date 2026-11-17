package executor

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor/tactics"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// rejection carries the wire reason code alongside the message the strategy
// receives, so reason codes are never recovered from error text.
type rejection struct {
	code   string
	reason string
	cause  error
}

func (e rejection) Error() string { return e.reason }
func (e rejection) Unwrap() error { return e.cause }

func reject(code, format string, args ...any) rejection {
	return rejection{code: code, reason: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) rejection {
	return reject(protocol.ReasonInvalidIntent, format, args...)
}

func validateIntentAt(intent protocol.ExecutionIntent, now time.Time) error {
	if intent.SchemaVersion != protocol.SchemaVersionV1 {
		return invalid("unsupported intent schema version %q", intent.SchemaVersion)
	}
	if intent.IntentID == "" || strings.TrimSpace(intent.UniqueTag) == "" || intent.Strategy == "" || intent.ConditionID == "" || intent.TokenID == "" || intent.Outcome == "" {
		return invalid("intent ID, unique tag, strategy, condition ID, token ID, and outcome are required")
	}
	if intent.Side != protocol.SideBuy && intent.Side != protocol.SideSell {
		return invalid("invalid side %q", intent.Side)
	}
	if intent.Kind != protocol.IntentOpen && intent.Kind != protocol.IntentClose {
		return invalid("invalid intent kind %q", intent.Kind)
	}
	if intent.Kind == protocol.IntentClose && intent.Side != protocol.SideSell {
		return invalid("close execution intent must sell")
	}
	if intent.Kind == protocol.IntentOpen {
		if _, err := decimal.PositiveFloat(intent.TargetUSD); err != nil {
			return invalid("invalid target usd %q", intent.TargetUSD)
		}
	}
	if intent.Policy.Style == protocol.ExecutionStyleSplitPreserve && intent.Kind != protocol.IntentOpen {
		return invalid("%s applies only to an open", protocol.ExecutionStyleSplitPreserve)
	}
	// An inventory lane is traded only by the preserve sells executiond
	// places on it; its pairs leave through merges.
	if protocol.IsInventoryTag(intent.UniqueTag) && intent.Policy.Preserve == nil {
		return invalid("unique tag %q names an inventory lane", intent.UniqueTag)
	}
	if intent.Policy.Preserve != nil && intent.Policy.Style != protocol.ExecutionStyleSplitPreserve {
		return invalid("policy.preserve is set by executiond only")
	}
	if intent.Policy.Style == protocol.ExecutionStyleMultiLevelLimit {
		if _, err := parseLadder(intent); err != nil {
			return err
		}
	} else if intent.BottomPrice != "" || intent.ScalingMode != "" {
		// Both are stored for every style; refuse them here rather than as
		// an opaque insert failure on the numeric cast or range check.
		return invalid("bottom_price and scaling_mode apply only to %s", protocol.ExecutionStyleMultiLevelLimit)
	}
	if _, err := decimal.Price(intent.LimitPrice); err != nil {
		return invalid("invalid limit price %q", intent.LimitPrice)
	}
	if intent.TimeInForce != protocol.TimeInForceGTC && intent.TimeInForce != protocol.TimeInForceFOK && intent.TimeInForce != protocol.TimeInForceFAK && intent.TimeInForce != protocol.TimeInForceGTD {
		return invalid("invalid time in force %q", intent.TimeInForce)
	}
	if intent.Kind == protocol.IntentOpen {
		if !intent.ExpiresAt.IsZero() && !intent.ExpiresAt.After(now) {
			return invalid("execution intent expired at %s", intent.ExpiresAt.UTC().Format(time.RFC3339Nano))
		}
		if intent.ExpiresAt.IsZero() && intent.Policy.CompleteWithinMillis == 0 {
			return invalid("execution intent requires expires_at or complete_within_ms")
		}
		// A FAK or FOK is decided on arrival and never rests, so a window
		// after the ack would be a promise nothing keeps. TAKER_AGGRESSIVE
		// is sent as a FAK.
		if intent.Policy.RestAfterAckMillis > 0 && (intent.TimeInForce == protocol.TimeInForceFAK || intent.TimeInForce == protocol.TimeInForceFOK ||
			intent.Policy.Style == protocol.ExecutionStyleTakerAggressive) {
			return invalid("rest_after_ack_ms needs an order that rests")
		}
	}
	if _, err := tactics.ParseDrift(intent); err != nil {
		return invalid("%s", err)
	}
	if err := validatePolicyDurations(intent.Policy); err != nil {
		return err
	}
	if intent.Policy.MaxFeatureAgeMillis > 0 && (intent.FeatureCompletedAt.IsZero() || now.Sub(intent.FeatureCompletedAt) > time.Duration(intent.Policy.MaxFeatureAgeMillis)*time.Millisecond) {
		return invalid("execution intent feature is stale")
	}
	// A ladder's levels are planned as limit orders, so its policy is checked
	// as one.
	planned := intent
	if planned.Policy.Style == protocol.ExecutionStyleMultiLevelLimit {
		planned.Policy.Style = protocol.ExecutionStyleLimit
	}
	if err := tactics.ValidatePolicy(planned); err != nil {
		if tactics.UnsupportedStyle(err) {
			return rejection{code: protocol.ReasonUnsupportedStyle, reason: err.Error(), cause: err}
		}
		return rejection{code: protocol.ReasonInvalidIntent, reason: err.Error(), cause: err}
	}
	return nil
}

func validatePolicyDurations(policy protocol.ExecutionPolicy) error {
	if policy.CompleteWithinMillis < 0 || policy.CancelTimeoutMillis < 0 || policy.MaxFeatureAgeMillis < 0 || policy.RepriceIntervalMillis < 0 ||
		policy.QuoteMaxAgeMillis < 0 || policy.SoftCloseAfterMillis < 0 || policy.ForceCloseAfterMillis < 0 || policy.CancelReplaceTimeoutMillis < 0 ||
		policy.RestAfterAckMillis < 0 {
		return invalid("execution policy durations must not be negative")
	}
	if policy.MaxReprices < 0 {
		return invalid("execution policy max_reprices must not be negative")
	}
	if policy.RepriceIntervalMillis > 0 || policy.MaxReprices > 0 || (policy.PostOnlyCrossRetry && policy.Style != protocol.ExecutionStyleMultiLevelLimit) ||
		policy.SoftCloseAfterMillis > 0 || policy.ForceCloseAfterMillis > 0 {
		return reject(protocol.ReasonUnimplementedPolicy, "execution policy lifecycle controls are not implemented")
	}
	return nil
}

// reasonFor maps an execution error onto the wire status, reason code, and
// strategy-visible message.
func reasonFor(err error) (protocol.ResultStatus, string, string) {
	var declared rejection
	if errors.As(err, &declared) {
		status := protocol.ResultFailed
		if declared.code == protocol.ReasonExecutionFailed {
			status = protocol.ResultFailed
		}
		return status, declared.code, declared.reason
	}
	var rejected *clobclient.OrderRejectedError
	if errors.As(err, &rejected) {
		return protocol.ResultFailed, protocol.ReasonOrderRejected, rejected.Message
	}
	return protocol.ResultFailed, protocol.ReasonExecutionFailed, "execution failed"
}

// reservationRecord describes the exposure a planned child order locks.
func reservationRecord(intent protocol.ExecutionIntent, child plannedChild, reservationID string, now time.Time) store.ReservationRecord {
	notional := "0"
	if product, ok := decimal.MulString(child.Shares, child.Price); ok {
		notional = product
	}
	return store.ReservationRecord{
		ReservationID: reservationID, IntentID: intent.IntentID, ChildSequence: child.Sequence, UniqueTag: intent.UniqueTag,
		MarketID: intent.MarketID, ConditionID: intent.ConditionID, TokenID: intent.TokenID, Outcome: intent.Outcome,
		Side: store.Side(intent.Side), Shares: child.Shares, Notional: notional, State: "active",
		Reason: child.ReservationReason, CreatedAt: now, UpdatedAt: now,
	}
}

func reservationID(intentID string, childSequence int) string {
	return fmt.Sprintf("%s:%d", intentID, childSequence)
}

func userOrder(intent protocol.ExecutionIntent, child plannedChild) (clobclient.UserOrder, error) {
	shares, err := decimal.FloorSharesFloat(child.Shares, clobclient.SharePrecisionDigits(intent.Side, child.TimeInForce))
	if err != nil {
		return clobclient.UserOrder{}, invalid("invalid planned shares %q", child.Shares)
	}
	price, err := strconv.ParseFloat(child.Price, 64)
	if err != nil || price <= 0 || price >= 1 {
		return clobclient.UserOrder{}, invalid("invalid planned price %q", child.Price)
	}
	return clobclient.UserOrder{TokenID: intent.TokenID, Side: intent.Side, Shares: shares, Price: price, PostOnly: child.PostOnly, OrderType: child.TimeInForce}, nil
}
