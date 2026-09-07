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
		IntentID: intent.IntentID, IdempotencyKey: intent.IdempotencyKey, Strategy: intent.Strategy, MarketID: intent.MarketID,
		Kind: store.IntentKind(intent.Kind), EventSlug: intent.EventSlug, ConditionID: intent.ConditionID, TokenID: intent.TokenID, Outcome: intent.Outcome,
		Side: store.Side(intent.Side), TargetShares: intent.TargetShares, LimitPrice: intent.LimitPrice, TimeInForce: store.TimeInForce(intent.TimeInForce),
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
		SchemaVersion: protocol.SchemaVersionV1, IntentID: record.IntentID, IdempotencyKey: record.IdempotencyKey, Strategy: record.Strategy,
		Kind: protocol.IntentKind(record.Kind), MarketID: record.MarketID, EventSlug: record.EventSlug,
		ConditionID: record.ConditionID, TokenID: record.TokenID, Outcome: record.Outcome, Side: protocol.Side(record.Side),
		TargetShares: record.TargetShares, LimitPrice: record.LimitPrice, TimeInForce: protocol.TimeInForce(record.TimeInForce),
		PostOnly: record.PostOnly, FeatureSeq: record.FeatureSeq, FeatureCompletedAt: record.FeatureCompletedAt,
		ExpiresAt: record.ExpiresAt, Policy: policy,
	}
}

// SameIntent reports whether a wire intent is the same logical intent as the
// durable record, used for idempotent redelivery detection. The policy is
// compared on its normalized JSON form.
func SameIntent(intent protocol.ExecutionIntent, record store.OrderIntentRecord) bool {
	if intent.IntentID != record.IntentID || intent.IdempotencyKey != record.IdempotencyKey || intent.Strategy != record.Strategy ||
		intent.Kind != protocol.IntentKind(record.Kind) || intent.MarketID != record.MarketID || intent.EventSlug != record.EventSlug ||
		intent.ConditionID != record.ConditionID || intent.TokenID != record.TokenID || intent.Outcome != record.Outcome ||
		intent.Side != protocol.Side(record.Side) || !SameDecimal(intent.TargetShares, record.TargetShares) || !SameDecimal(intent.LimitPrice, record.LimitPrice) ||
		intent.TimeInForce != protocol.TimeInForce(record.TimeInForce) || intent.PostOnly != record.PostOnly || intent.FeatureSeq != record.FeatureSeq ||
		!intent.FeatureCompletedAt.Equal(record.FeatureCompletedAt) || !intent.ExpiresAt.Equal(record.ExpiresAt) {
		return false
	}
	return policyEqual(intent.Policy, record.Policy)
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

// TerminalAck derives the strategy-facing terminal intent acknowledgement for
// an order that reached a terminal state.
func TerminalAck(order store.SignedOrderRecord, reason string, occurredAt time.Time) (protocol.ExecutionIntentAck, bool) {
	ack := protocol.ExecutionIntentAck{IntentID: order.IntentID, Reason: reason, FilledShares: order.MatchedShares, OccurredAt: occurredAt}
	switch order.State {
	case statemachine.StateFilled:
		ack.Status = protocol.IntentCompleted
	case statemachine.StateCanceled:
		ack.Status = protocol.IntentExpired
		if decimal.Positive(order.MatchedShares) {
			ack.Status = protocol.IntentPartial
		}
	case statemachine.StateRejected:
		ack.Status = protocol.IntentRejected
		ack.ReasonCode = "ORDER_REJECTED"
	case statemachine.StateExpired:
		ack.Status = protocol.IntentExpired
	case statemachine.StateFailed:
		ack.Status = protocol.IntentFailed
		ack.ReasonCode = "EXECUTION_FAILED"
	default:
		return protocol.ExecutionIntentAck{}, false
	}
	return ack, true
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
		MarketID:          position.MarketID,
		ConditionID:       position.ConditionID,
		TokenID:           position.TokenID,
		Outcome:           position.Outcome,
		HasPosition:       entryPrice != nil,
		EntryPrice:        entryPrice,
		EntryTime:         entryTime,
		SecondsSinceEntry: secondsSinceEntry,
		PositionSize:      position.PositionSize,
		AvailableSize:     position.AvailableSize,
		ReservedSize:      position.ReservedSize,
		State:             position.State,
		SourceRevision:    position.SourceRevision,
		UpdatedAt:         position.UpdatedAt.UTC(),
		PublishedAt:       publishedAt.UTC(),
	}
}

func policyEqual(wire protocol.ExecutionPolicy, stored []byte) bool {
	encoded, err := json.Marshal(wire)
	if err != nil {
		return false
	}
	return bytes.Equal(encoded, stored)
}
