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
	feature, err := PositionFeature(store.PositionRecord{
		MarketID:       "market",
		ConditionID:    "condition",
		TokenID:        "token",
		UniqueTag:      "lane-a",
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
	if err != nil {
		t.Fatalf("position feature: %v", err)
	}

	if !feature.HasPosition {
		t.Fatal("expected open position")
	}
	if feature.UniqueTag != "lane-a" {
		t.Fatalf("unique tag=%q", feature.UniqueTag)
	}
	if feature.EntryPrice == nil || *feature.EntryPrice != 0.42 {
		t.Fatalf("entry price=%v", feature.EntryPrice)
	}
	if feature.EntryTime == nil || !feature.EntryTime.Equal(entryTime) {
		t.Fatalf("entry time=%v", feature.EntryTime)
	}
	if feature.SecondsSinceEntry != 30 {
		t.Fatalf("seconds since entry=%v", feature.SecondsSinceEntry)
	}
	if feature.PositionSize != 5.5 || feature.AvailableSize != 4.5 || feature.ReservedSize != 1 {
		t.Fatalf("unexpected sizes: %+v", feature)
	}
}

func TestPositionFeatureEmptyPosition(t *testing.T) {
	feature, err := PositionFeature(store.PositionRecord{
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
	if err != nil {
		t.Fatalf("position feature: %v", err)
	}

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
		UniqueTag:     "lane-a",
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
	if record.Side != store.SideBuy || record.Kind != store.IntentOpen || record.TimeInForce != store.TimeInForceGTC || record.UniqueTag != "lane-a" {
		t.Fatalf("unexpected record types: %+v", record)
	}
	if len(record.Policy) == 0 {
		t.Fatal("expected serialized policy")
	}
	round := ExecutionIntent(record)
	if round.Policy.CompleteWithinMillis != 60_000 || round.Side != protocol.SideBuy || round.TargetUSD != "1.5" || round.UniqueTag != "lane-a" {
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
		result, ok := TerminalResult(store.SignedOrderRecord{IntentID: "intent", ChildSequence: store.StrategyChildSequence, State: tc.state, MatchedShares: "0"}, openIntent, "reason", "", at)
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
	result, ok := TerminalResult(store.SignedOrderRecord{IntentID: "intent", ChildSequence: store.StrategyChildSequence, State: statemachine.StateCanceled, MatchedShares: "0.5"}, openIntent, "reason", "", time.Unix(100, 0).UTC())
	if !ok || result.Status != protocol.ResultSucceeded {
		t.Fatalf("expected successful result, got ok=%v result=%+v", ok, result)
	}
}

// A force-close exit reaching a terminal state is not the intent's outcome.
func TestTerminalResultIgnoresInternalChildren(t *testing.T) {
	openIntent := store.OrderIntentRecord{IntentID: "intent", Kind: store.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy}
	if _, ok := TerminalResult(store.SignedOrderRecord{IntentID: "intent", ChildSequence: store.StrategyChildSequence + 1, State: statemachine.StateFilled, MatchedShares: "2"}, openIntent, "reason", "", time.Unix(100, 0).UTC()); ok {
		t.Fatal("expected no terminal result for an internal child order")
	}
}

// A partially filled order that is still working is not terminal and must not
// produce a result; it resolves to a terminal state later.
func TestTerminalResultPartiallyFilledIsNotTerminal(t *testing.T) {
	openIntent := store.OrderIntentRecord{IntentID: "intent", Kind: store.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy}
	if _, ok := TerminalResult(store.SignedOrderRecord{IntentID: "intent", ChildSequence: store.StrategyChildSequence, State: statemachine.StatePartiallyFilled, MatchedShares: "0.5"}, openIntent, "reason", "", time.Unix(100, 0).UTC()); ok {
		t.Fatal("expected no terminal result for a still-working partially filled order")
	}
}

func TestTerminalCloseResultMapsStates(t *testing.T) {
	at := time.Unix(100, 0).UTC()
	closeIntent := store.OrderIntentRecord{IntentID: "intent", Kind: store.IntentClose, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideSell}
	cases := []struct {
		state  statemachine.State
		status protocol.ResultStatus
		ok     bool
	}{
		{statemachine.StateFilled, protocol.ResultSucceeded, true},
		{statemachine.StateCanceled, protocol.ResultFailed, true},
		{statemachine.StateRejected, protocol.ResultFailed, true},
		{statemachine.StateExpired, protocol.ResultFailed, true},
		{statemachine.StateFailed, protocol.ResultFailed, true},
		{statemachine.StateLive, "", false},
	}
	for _, tc := range cases {
		result, ok := TerminalCloseResult(store.SignedOrderRecord{IntentID: "intent", ChildSequence: store.StrategyChildSequence, State: tc.state, MatchedShares: "0"}, closeIntent, "reason", "", at)
		if ok != tc.ok {
			t.Fatalf("state %s ok=%v want %v", tc.state, ok, tc.ok)
		}
		if !ok {
			continue
		}
		if result.Status != tc.status {
			t.Fatalf("state %s status=%s want %s", tc.state, result.Status, tc.status)
		}
		if result.AssetID != "token" || result.Side != protocol.SideSell {
			t.Fatalf("unexpected close identity: %+v", result)
		}
	}
}

func TestTerminalResultsCarryAveragePriceOnlyWhenFilled(t *testing.T) {
	at := time.Unix(100, 0).UTC()
	closeIntent := store.OrderIntentRecord{IntentID: "intent", Kind: store.IntentClose, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideSell}
	filled, ok := TerminalCloseResult(store.SignedOrderRecord{IntentID: "intent", ChildSequence: store.StrategyChildSequence, State: statemachine.StateFilled, MatchedShares: "7.5"}, closeIntent, "reason", "0.685", at)
	if !ok || filled.AveragePrice != 0.685 {
		t.Fatalf("expected average_price 0.685 on a filled close, got ok=%v %+v", ok, filled)
	}
	unfilled, ok := TerminalCloseResult(store.SignedOrderRecord{IntentID: "intent", ChildSequence: store.StrategyChildSequence, State: statemachine.StateCanceled, MatchedShares: "0"}, closeIntent, "reason", "0.685", at)
	if !ok || unfilled.AveragePrice != 0 {
		t.Fatalf("expected no average_price without fills, got ok=%v %+v", ok, unfilled)
	}
	openIntent := store.OrderIntentRecord{IntentID: "intent", Kind: store.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy}
	open, ok := TerminalResult(store.SignedOrderRecord{IntentID: "intent", ChildSequence: store.StrategyChildSequence, State: statemachine.StateFilled, MatchedShares: "7.5"}, openIntent, "reason", "not-a-price", at)
	if !ok || open.AveragePrice != 0 {
		t.Fatalf("expected an invalid price to be omitted, got ok=%v %+v", ok, open)
	}
}

func TestTerminalCloseResultIgnoresOpenIntentAndInternalChild(t *testing.T) {
	at := time.Unix(100, 0).UTC()
	openIntent := store.OrderIntentRecord{IntentID: "intent", Kind: store.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy}
	if _, ok := TerminalCloseResult(store.SignedOrderRecord{IntentID: "intent", ChildSequence: store.StrategyChildSequence, State: statemachine.StateFilled, MatchedShares: "2"}, openIntent, "reason", "", at); ok {
		t.Fatal("expected no close result for an open intent")
	}
	closeIntent := store.OrderIntentRecord{IntentID: "intent", Kind: store.IntentClose, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideSell}
	if _, ok := TerminalCloseResult(store.SignedOrderRecord{IntentID: "intent", ChildSequence: store.StrategyChildSequence + 1, State: statemachine.StateFilled, MatchedShares: "2"}, closeIntent, "reason", "", at); ok {
		t.Fatal("expected no close result for a force-close exit")
	}
}
