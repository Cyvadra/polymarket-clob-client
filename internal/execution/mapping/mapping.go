// Package mapping converts between the durable store record shapes and the
// versioned NATS wire types. It owns every protocol<->store boundary so that
// pkg/store stays a pure persistence package with no dependency on the wire
// schema (which is versioned independently in docs/protocol/nats-v1.md).
package mapping

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// IntentRecord maps a wire ExecutionIntent onto the durable intent record.
// The policy is persisted as opaque JSON so the store schema never changes
// when the wire policy gains or drops fields.
func IntentRecord(intent protocol.ExecutionIntent, now time.Time) store.OrderIntentRecord {
	policy, _ := json.Marshal(intent.Policy)
	if bytes.Equal(bytes.TrimSpace(policy), []byte("{}")) {
		policy = nil
	}
	return store.OrderIntentRecord{
		IntentID: intent.IntentID, RequestID: intent.RequestID, UniqueTag: intent.UniqueTag, Strategy: intent.Strategy, MarketID: intent.MarketID,
		Kind: store.IntentKind(intent.Kind), EventSlug: intent.EventSlug, ConditionID: intent.ConditionID, TokenID: intent.TokenID, Outcome: intent.Outcome,
		Side: store.Side(intent.Side), TargetUSD: intent.TargetUSD, TargetEquityFraction: intent.TargetEquityFraction, SizedEquityUSD: intent.SizedEquityUSD, LimitPrice: intent.LimitPrice, BottomPrice: intent.BottomPrice, ScalingMode: intent.ScalingMode, TimeInForce: store.TimeInForce(intent.TimeInForce),
		PostOnly: intent.PostOnly, FeatureSeq: intent.FeatureSeq, FeatureCompletedAt: intent.FeatureCompletedAt, ExpiresAt: intent.ExpiresAt,
		SignalPrice: intent.SignalPrice, MaxSignalDrift: intent.MaxSignalDrift,
		Status: statemachine.StateIntentReceived, Policy: policy, CreatedAt: now, UpdatedAt: now,
	}
}

// ExecutionIntent maps a durable intent record back onto the wire shape.
func ExecutionIntent(record store.OrderIntentRecord) protocol.ExecutionIntent {
	var policy protocol.ExecutionPolicy
	if len(record.Policy) > 0 {
		_ = json.Unmarshal(record.Policy, &policy)
	}
	return protocol.ExecutionIntent{
		SchemaVersion: protocol.SchemaVersionV1, IntentID: record.IntentID, RequestID: record.RequestID, UniqueTag: record.UniqueTag, Strategy: record.Strategy,
		Kind: protocol.IntentKind(record.Kind), MarketID: record.MarketID, EventSlug: record.EventSlug,
		ConditionID: record.ConditionID, TokenID: record.TokenID, Outcome: record.Outcome, Side: protocol.Side(record.Side),
		TargetUSD: record.TargetUSD, LimitPrice: record.LimitPrice, BottomPrice: record.BottomPrice, ScalingMode: record.ScalingMode, TimeInForce: protocol.TimeInForce(record.TimeInForce),
		PostOnly: record.PostOnly, FeatureSeq: record.FeatureSeq, FeatureCompletedAt: record.FeatureCompletedAt,
		ExpiresAt: record.ExpiresAt, SignalPrice: record.SignalPrice, MaxSignalDrift: record.MaxSignalDrift, Policy: policy,
	}
}

// SameDecimal compares two decimal strings numerically, falling back to a
// trimmed textual comparison when either side is not a valid decimal.
func SameDecimal(left, right string) bool {
	leftRat, leftOK := decimal.Rat(left)
	rightRat, rightOK := decimal.Rat(right)
	if !leftOK || !rightOK {
		return strings.TrimSpace(left) == strings.TrimSpace(right)
	}
	return leftRat.Cmp(rightRat) == 0
}

// terminalStatus maps a terminal order state onto the strategy-facing result
// status and reason code. A still-working order (for example a partially
// filled GTC order) returns ok=false so no result is produced until the order
// actually resolves.
func terminalStatus(state statemachine.State, matchedShares string) (protocol.ResultStatus, string, bool) {
	switch state {
	case statemachine.StateFilled:
		return protocol.ResultSucceeded, "", true
	case statemachine.StateCanceled:
		if decimal.Positive(matchedShares) {
			return protocol.ResultSucceeded, "", true
		}
		return protocol.ResultFailed, "", true
	case statemachine.StateRejected:
		return protocol.ResultFailed, protocol.ReasonOrderRejected, true
	case statemachine.StateExpired:
		return protocol.ResultFailed, "", true
	case statemachine.StateFailed:
		return protocol.ResultFailed, protocol.ReasonExecutionFailed, true
	default:
		return "", "", false
	}
}

// unfilledReason names why an order ended with nothing filled, which
// terminalStatus leaves blank: the cause executiond marked on the intent
// before cancelling (SIGNAL_DRIFT, or EXPIRED for its deadline), EXPIRED for
// an order the exchange expired, and CANCELED for any other cancel. A code
// terminalStatus already gave, and every success, pass through.
func unfilledReason(status protocol.ResultStatus, code string, state statemachine.State, intentStatus statemachine.State) string {
	if status != protocol.ResultFailed || code != "" {
		return code
	}
	switch {
	case intentStatus == store.IntentStatusSignalDrift:
		return protocol.ReasonSignalDrift
	case intentStatus == store.IntentStatusDeadline, state == statemachine.StateExpired:
		return protocol.ReasonExpired
	case state == statemachine.StateCanceled:
		return protocol.ReasonCanceled
	}
	return code
}

// TerminalResult derives the strategy-facing open result for an order of an
// OPEN intent that reached a terminal state. Only the strategy child of an
// open intent produces a result: internal children (a force-close exit) and
// close intents are never surfaced as an open result. The result carries the
// position identity from the parent intent so the strategy can attribute it
// without a client-supplied correlation key.
//
// averagePrice is the fill-weighted price of the order's matches when the
// caller knows it; an empty or invalid value leaves average_price omitted.
func TerminalResult(order store.SignedOrderRecord, intent store.OrderIntentRecord, reason, averagePrice string, occurredAt time.Time) (protocol.ExecutionOpenResult, bool) {
	if order.ChildSequence != store.StrategyChildSequence || intent.Kind != store.IntentOpen {
		return protocol.ExecutionOpenResult{}, false
	}
	status, reasonCode, ok := terminalStatus(order.State, order.MatchedShares)
	if !ok {
		return protocol.ExecutionOpenResult{}, false
	}
	reasonCode = unfilledReason(status, reasonCode, order.State, intent.Status)
	filledShares, err := decimal.NonNegativeFloat(order.MatchedShares)
	if err != nil {
		return protocol.ExecutionOpenResult{}, false
	}
	return protocol.ExecutionOpenResult{
		RequestID: intent.RequestID, UniqueTag: intent.UniqueTag, Strategy: intent.Strategy, ConditionID: intent.ConditionID, TokenID: intent.TokenID, Outcome: intent.Outcome,
		Side: protocol.Side(intent.Side), Status: status, ReasonCode: reasonCode, Reason: reason,
		FilledShares: filledShares, AveragePrice: resultPrice(averagePrice, filledShares), OccurredAt: occurredAt,
	}, true
}

// LadderChild is one child order of a multi-level intent with the
// fill-weighted price of its matches, empty when the caller has none.
type LadderChild struct {
	Order        store.SignedOrderRecord
	AveragePrice string
}

// AggregateOpenResult folds the children of a multi-level OPEN intent into the
// one result the strategy receives. Every child must be terminal. The result
// succeeds when anything filled, with the shares summed and the price weighted
// by shares. When nothing filled it fails with the reason of the child closest
// to the limit price (the highest sequence), the level the strategy most
// wanted, subject to the same drift and deadline markers a single order obeys.
func AggregateOpenResult(intent store.OrderIntentRecord, children []LadderChild, reason string, occurredAt time.Time) (protocol.ExecutionOpenResult, error) {
	if intent.Kind != store.IntentOpen {
		return protocol.ExecutionOpenResult{}, fmt.Errorf("intent %s is not an open", intent.IntentID)
	}
	if len(children) == 0 {
		return protocol.ExecutionOpenResult{}, fmt.Errorf("intent %s has no child orders to aggregate", intent.IntentID)
	}
	shares, notional := new(big.Rat), new(big.Rat)
	var lead *LadderChild
	for i := range children {
		child := &children[i]
		if _, _, ok := terminalStatus(child.Order.State, child.Order.MatchedShares); !ok {
			return protocol.ExecutionOpenResult{}, fmt.Errorf("child %d of intent %s is %s, not terminal", child.Order.ChildSequence, intent.IntentID, child.Order.State)
		}
		if lead == nil || child.Order.ChildSequence > lead.Order.ChildSequence {
			lead = child
		}
		matched, ok := decimal.Rat(child.Order.MatchedShares)
		if !ok || matched.Sign() < 0 {
			return protocol.ExecutionOpenResult{}, fmt.Errorf("child %d of intent %s has invalid matched shares %q", child.Order.ChildSequence, intent.IntentID, child.Order.MatchedShares)
		}
		if matched.Sign() == 0 {
			continue
		}
		price, ok := decimal.Rat(child.AveragePrice)
		if !ok || price.Sign() <= 0 {
			// The fill ledger has not priced this child: the limit it rested
			// at bounds what a post-only fill could have cost.
			if price, ok = decimal.Rat(child.Order.Price); !ok {
				return protocol.ExecutionOpenResult{}, fmt.Errorf("child %d of intent %s has no usable price", child.Order.ChildSequence, intent.IntentID)
			}
		}
		shares.Add(shares, matched)
		notional.Add(notional, new(big.Rat).Mul(matched, price))
	}
	result := protocol.ExecutionOpenResult{
		RequestID: intent.RequestID, UniqueTag: intent.UniqueTag, Strategy: intent.Strategy, ConditionID: intent.ConditionID, TokenID: intent.TokenID, Outcome: intent.Outcome,
		Side: protocol.Side(intent.Side), Reason: reason, OccurredAt: occurredAt,
	}
	if shares.Sign() > 0 {
		result.Status = protocol.ResultSucceeded
		result.FilledShares, _ = shares.Float64()
		average, _ := new(big.Rat).Quo(notional, shares).Float64()
		result.AveragePrice = average
		return result, nil
	}
	status, code, _ := terminalStatus(lead.Order.State, lead.Order.MatchedShares)
	result.Status, result.ReasonCode = status, unfilledReason(status, code, lead.Order.State, intent.Status)
	return result, nil
}

// resultPrice converts an average fill price for a result. A result with no
// filled shares has no meaningful price, so it is omitted.
func resultPrice(averagePrice string, filledShares float64) float64 {
	if filledShares <= 0 {
		return 0
	}
	price, err := decimal.Price(averagePrice)
	if err != nil {
		return 0
	}
	return price
}

// TerminalCloseResult derives the strategy-facing close result for the
// strategy child of a CLOSE intent that reached a terminal state. Only the
// strategy child (sequence 1) produces a close result: a close's force-close
// exit is an internal child (higher sequence) that never emits one, so the
// strategy reconciles a force close from position.features.* instead.
func TerminalCloseResult(order store.SignedOrderRecord, intent store.OrderIntentRecord, reason, averagePrice string, occurredAt time.Time) (protocol.ExecutionCloseResult, bool) {
	if order.ChildSequence != store.StrategyChildSequence || intent.Kind != store.IntentClose {
		return protocol.ExecutionCloseResult{}, false
	}
	status, reasonCode, ok := terminalStatus(order.State, order.MatchedShares)
	if !ok {
		return protocol.ExecutionCloseResult{}, false
	}
	reasonCode = unfilledReason(status, reasonCode, order.State, intent.Status)
	filledShares, err := decimal.NonNegativeFloat(order.MatchedShares)
	if err != nil {
		return protocol.ExecutionCloseResult{}, false
	}
	return protocol.ExecutionCloseResult{
		RequestID: intent.RequestID, Mode: CloseMode(intent), UniqueTag: intent.UniqueTag, Strategy: intent.Strategy, ConditionID: intent.ConditionID, AssetID: intent.TokenID, Outcome: intent.Outcome,
		Side: protocol.SideSell, Status: status, ReasonCode: reasonCode, Reason: reason,
		FilledShares: filledShares, AveragePrice: resultPrice(averagePrice, filledShares), OccurredAt: occurredAt,
	}, true
}

// PositionFeature maps a durable position record onto the strategy-facing
// position feature published under position.features.<condition_id>.<token_id>.
func PositionFeature(position store.PositionRecord, sequence int64, publishedAt time.Time) (protocol.PositionFeature, error) {
	positionSize, err := wireNonNegative(position.PositionSize)
	if err != nil {
		return protocol.PositionFeature{}, fmt.Errorf("position_size: %w", err)
	}
	actualShares, err := wireNonNegative(position.ActualShares)
	if err != nil {
		return protocol.PositionFeature{}, fmt.Errorf("actual_shares: %w", err)
	}
	availableSize, err := wireNonNegative(position.AvailableSize)
	if err != nil {
		return protocol.PositionFeature{}, fmt.Errorf("available_size: %w", err)
	}
	reservedSize, err := wireNonNegative(position.ReservedSize)
	if err != nil {
		return protocol.PositionFeature{}, fmt.Errorf("reserved_size: %w", err)
	}
	var entryPrice *float64
	if position.EntryPrice != "" && decimal.Positive(position.PositionSize) {
		parsed, err := decimal.Price(position.EntryPrice)
		if err != nil {
			return protocol.PositionFeature{}, fmt.Errorf("entry_price: %w", err)
		}
		entryPrice = &parsed
	}

	var entryTime *time.Time
	secondsSinceEntry := 0.0
	if entryPrice != nil && !position.EntryTime.IsZero() {
		entry := position.EntryTime.UTC()
		entryTime = &entry
		if publishedAt.After(entry) {
			secondsSinceEntry = publishedAt.Sub(entry).Seconds()
		}
	}

	return protocol.PositionFeature{
		SchemaVersion:     protocol.SchemaVersionV1,
		Seq:               sequence,
		UniqueTag:         position.UniqueTag,
		MarketID:          position.MarketID,
		ConditionID:       position.ConditionID,
		TokenID:           position.TokenID,
		Outcome:           position.Outcome,
		HasPosition:       entryPrice != nil,
		EntryPrice:        entryPrice,
		EntryTime:         entryTime,
		SecondsSinceEntry: secondsSinceEntry,
		PositionSize:      positionSize,
		OpenLots:          position.OpenLots,
		ActualShares:      actualShares,
		AvailableSize:     availableSize,
		ReservedSize:      reservedSize,
		State:             position.State,
		SourceRevision:    position.SourceRevision,
		UpdatedAt:         position.UpdatedAt.UTC(),
		PublishedAt:       publishedAt.UTC(),
	}, nil
}

func wireNonNegative(value string) (float64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	return decimal.NonNegativeFloat(value)
}

// CloseMode names the close mode a close intent was placed in. A force close
// is the one placed fill-and-kill (see Executor.closeIntent); every other
// close intent carries the strategy's own limit.
func CloseMode(intent store.OrderIntentRecord) protocol.ExecutionCloseMode {
	if intent.TimeInForce == store.TimeInForce(protocol.TimeInForceFAK) {
		return protocol.ExecutionCloseModeForce
	}
	return protocol.ExecutionCloseModeLimit
}
