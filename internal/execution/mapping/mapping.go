// Package mapping converts between the durable store record shapes and the
// versioned NATS wire types. It owns every protocol<->store boundary so that
// pkg/store stays a pure persistence package with no dependency on the wire
// schema (which is versioned independently in docs/protocol/nats-v1.md).
package mapping

import (
	"bytes"
	"encoding/json"
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
		IntentID: intent.IntentID, UniqueTag: intent.UniqueTag, Strategy: intent.Strategy, MarketID: intent.MarketID,
		Kind: store.IntentKind(intent.Kind), EventSlug: intent.EventSlug, ConditionID: intent.ConditionID, TokenID: intent.TokenID, Outcome: intent.Outcome,
		Side: store.Side(intent.Side), TargetUSD: intent.TargetUSD, LimitPrice: intent.LimitPrice, TimeInForce: store.TimeInForce(intent.TimeInForce),
		PostOnly: intent.PostOnly, FeatureSeq: intent.FeatureSeq, FeatureCompletedAt: intent.FeatureCompletedAt, ExpiresAt: intent.ExpiresAt,
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
		SchemaVersion: protocol.SchemaVersionV1, IntentID: record.IntentID, UniqueTag: record.UniqueTag, Strategy: record.Strategy,
		Kind: protocol.IntentKind(record.Kind), MarketID: record.MarketID, EventSlug: record.EventSlug,
		ConditionID: record.ConditionID, TokenID: record.TokenID, Outcome: record.Outcome, Side: protocol.Side(record.Side),
		TargetUSD: record.TargetUSD, LimitPrice: record.LimitPrice, TimeInForce: protocol.TimeInForce(record.TimeInForce),
		PostOnly: record.PostOnly, FeatureSeq: record.FeatureSeq, FeatureCompletedAt: record.FeatureCompletedAt,
		ExpiresAt: record.ExpiresAt, Policy: policy,
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

// TerminalResult derives the strategy-facing open result for an order of an
// OPEN intent that reached a terminal state. Only the strategy child of an
// open intent produces a result: internal children (a force-close exit) and
// close intents are never surfaced as an open result. The result carries the
// position identity from the parent intent so the strategy can attribute it
// without a client-supplied correlation key.
func TerminalResult(order store.SignedOrderRecord, intent store.OrderIntentRecord, reason string, occurredAt time.Time) (protocol.ExecutionOpenResult, bool) {
	if order.ChildSequence != store.StrategyChildSequence || intent.Kind != store.IntentOpen {
		return protocol.ExecutionOpenResult{}, false
	}
	status, reasonCode, ok := terminalStatus(order.State, order.MatchedShares)
	if !ok {
		return protocol.ExecutionOpenResult{}, false
	}
	return protocol.ExecutionOpenResult{
		UniqueTag: intent.UniqueTag, ConditionID: intent.ConditionID, TokenID: intent.TokenID, Outcome: intent.Outcome,
		Side: protocol.Side(intent.Side), Status: status, ReasonCode: reasonCode, Reason: reason,
		FilledShares: order.MatchedShares, OccurredAt: occurredAt,
	}, true
}

// TerminalCloseResult derives the strategy-facing close result for the
// strategy child of a CLOSE intent that reached a terminal state. Only the
// strategy child (sequence 1) produces a close result: a close's force-close
// exit is an internal child (higher sequence) that never emits one, so the
// strategy reconciles a force close from position.features.* instead.
func TerminalCloseResult(order store.SignedOrderRecord, intent store.OrderIntentRecord, reason string, occurredAt time.Time) (protocol.ExecutionCloseResult, bool) {
	if order.ChildSequence != store.StrategyChildSequence || intent.Kind != store.IntentClose {
		return protocol.ExecutionCloseResult{}, false
	}
	status, reasonCode, ok := terminalStatus(order.State, order.MatchedShares)
	if !ok {
		return protocol.ExecutionCloseResult{}, false
	}
	return protocol.ExecutionCloseResult{
		UniqueTag: intent.UniqueTag, ConditionID: intent.ConditionID, AssetID: intent.TokenID, Outcome: intent.Outcome,
		Side: protocol.SideSell, Status: status, ReasonCode: reasonCode, Reason: reason,
		FilledShares: order.MatchedShares, OccurredAt: occurredAt,
	}, true
}

// PositionFeature maps a durable position record onto the strategy-facing
// position feature published under position.features.<condition_id>.<token_id>.
func PositionFeature(position store.PositionRecord, sequence int64, publishedAt time.Time) protocol.PositionFeature {
	var entryPrice *string
	if position.EntryPrice != "" && decimal.Positive(position.PositionSize) {
		entryPrice = &position.EntryPrice
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
		PositionSize:      position.PositionSize,
		ActualShares:      position.ActualShares,
		AvailableSize:     position.AvailableSize,
		ReservedSize:      position.ReservedSize,
		State:             position.State,
		SourceRevision:    position.SourceRevision,
		UpdatedAt:         position.UpdatedAt.UTC(),
		PublishedAt:       publishedAt.UTC(),
	}
}
