package mapping

import (
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

func TestPositionFeatureOpenPosition(t *testing.T) {
	entryTime := time.Unix(100, 0).UTC()
	publishedAt := time.Unix(130, 0).UTC()
	feature := PositionFeature(store.PositionRecord{
		MarketID:       "market",
		ConditionID:    "condition",
		TokenID:        "token",
		Outcome:        "Up",
		PositionSize:   "5.5",
		AvailableSize:  "4.5",
		ReservedSize:   "1",
		EntryPrice:     "0.42",
		EntryTime:      entryTime,
		State:          "confirmed",
		SourceRevision: 9,
		UpdatedAt:      time.Unix(120, 0).UTC(),
	}, 11, publishedAt)

	if !feature.HasPosition {
		t.Fatal("expected open position")
	}
	if feature.EntryPrice == nil || *feature.EntryPrice != "0.42" {
		t.Fatalf("entry price=%v", feature.EntryPrice)
	}
	if feature.EntryTime == nil || !feature.EntryTime.Equal(entryTime) {
		t.Fatalf("entry time=%v", feature.EntryTime)
	}
	if feature.SecondsSinceEntry != 30 {
		t.Fatalf("seconds since entry=%v", feature.SecondsSinceEntry)
	}
	if feature.PositionSize != "5.5" || feature.AvailableSize != "4.5" || feature.ReservedSize != "1" {
		t.Fatalf("unexpected sizes: %+v", feature)
	}
}

func TestPositionFeatureEmptyPosition(t *testing.T) {
	feature := PositionFeature(store.PositionRecord{
		ConditionID:   "condition",
		TokenID:       "token",
		Outcome:       "Down",
		PositionSize:  "0",
		AvailableSize: "0",
		ReservedSize:  "0",
		EntryPrice:    "0.50",
		EntryTime:     time.Unix(100, 0).UTC(),
		UpdatedAt:     time.Unix(120, 0).UTC(),
	}, 12, time.Unix(130, 0).UTC())

	if feature.HasPosition {
		t.Fatal("expected empty position")
	}
	if feature.EntryPrice != nil || feature.EntryTime != nil {
		t.Fatalf("expected nil entry fields, got %+v", feature)
	}
	if feature.SecondsSinceEntry != 0 {
		t.Fatalf("seconds since entry=%v", feature.SecondsSinceEntry)
	}
}

func TestIntentRecordRoundTrip(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	intent := protocol.ExecutionIntent{
		SchemaVersion: protocol.SchemaVersionV1,
		IntentID:      "intent",
		Strategy:      "strategy",
		Kind:          protocol.IntentOpen,
		ConditionID:   "condition",
		TokenID:       "token",
		Outcome:       "Up",
		Side:          protocol.SideBuy,
		TargetUSD:     "1.5",
		LimitPrice:    "0.5",
		TimeInForce:   protocol.TimeInForceGTC,
		Policy:        protocol.ExecutionPolicy{CompleteWithinMillis: 60_000},
		FeatureSeq:    7,
		ExpiresAt:     time.Unix(200, 0).UTC(),
	}
	record := IntentRecord(intent, now)
	if record.Side != store.SideBuy || record.Kind != store.IntentOpen || record.TimeInForce != store.TimeInForceGTC {
		t.Fatalf("unexpected record types: %+v", record)
	}
	if len(record.Policy) == 0 {
		t.Fatal("expected serialized policy")
	}
	round := ExecutionIntent(record)
	if round.Policy.CompleteWithinMillis != 60_000 || round.Side != protocol.SideBuy || round.TargetUSD != "1.5" {
		t.Fatalf("round trip mismatch: %+v", round)
	}
}

func TestTerminalResultMapsStates(t *testing.T) {
	at := time.Unix(100, 0).UTC()
	openIntent := store.OrderIntentRecord{IntentID: "intent", Kind: store.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy}
	cases := []struct {
		state    statemachine.State
		status   protocol.ResultStatus
		hasMatch bool
	}{
		{statemachine.StateFilled, protocol.ResultSucceeded, true},
		{statemachine.StateCanceled, protocol.ResultFailed, true},
		{statemachine.StateRejected, protocol.ResultFailed, true},
		{statemachine.StateExpired, protocol.ResultFailed, true},
		{statemachine.StateFailed, protocol.ResultFailed, true},
		{statemachine.StateLive, "", false},
	}
	for _, tc := range cases {
		result, ok := TerminalResult(store.SignedOrderRecord{IntentID: "intent", ChildSequence: store.StrategyChildSequence, State: tc.state, MatchedShares: "0"}, openIntent, "reason", at)
		if ok != tc.hasMatch {
			t.Fatalf("state %s ok=%v want %v", tc.state, ok, tc.hasMatch)
		}
		if !ok {
			continue
		}
		if result.Status != tc.status {
			t.Fatalf("state %s status=%s want %s", tc.state, result.Status, tc.status)
		}
	}
}

func TestTerminalResultPartialCancelCountsSuccess(t *testing.T) {
	openIntent := store.OrderIntentRecord{IntentID: "intent", Kind: store.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy}
	result, ok := TerminalResult(store.SignedOrderRecord{IntentID: "intent", ChildSequence: store.StrategyChildSequence, State: statemachine.StateCanceled, MatchedShares: "0.5"}, openIntent, "reason", time.Unix(100, 0).UTC())
	if !ok || result.Status != protocol.ResultSucceeded {
		t.Fatalf("expected successful result, got ok=%v result=%+v", ok, result)
	}
}

// A force-close exit reaching a terminal state is not the intent's outcome.
func TestTerminalResultIgnoresInternalChildren(t *testing.T) {
	openIntent := store.OrderIntentRecord{IntentID: "intent", Kind: store.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy}
	if _, ok := TerminalResult(store.SignedOrderRecord{IntentID: "intent", ChildSequence: store.StrategyChildSequence + 1, State: statemachine.StateFilled, MatchedShares: "2"}, openIntent, "reason", time.Unix(100, 0).UTC()); ok {
		t.Fatal("expected no terminal result for an internal child order")
	}
}
