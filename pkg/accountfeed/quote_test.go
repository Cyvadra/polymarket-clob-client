package accountfeed

import (
	"context"
	"testing"

	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

var quoteIntent = store.OrderIntentRecord{
	IntentID: "q", UniqueTag: "lane", Strategy: "s", Kind: store.IntentOpen, ConditionID: "c", TokenID: "t", Outcome: "Up", Side: store.SideBuy,
	Policy: []byte(`{"style":"QUOTE"}`),
}

func TestQuoteLevelsNeverPublishOpenResults(t *testing.T) {
	if !IsQuoteIntent(quoteIntent) || IsLadderIntent(quoteIntent) {
		t.Fatal("quote intent misread")
	}
	publisher := &failablePublisher{}
	order := store.SignedOrderRecord{IntentID: "q", ChildSequence: 3, State: statemachine.StateFilled, MatchedShares: "5"}
	if err := PublishTerminalResultWithStore(context.Background(), nil, publisher, quoteIntent, order, "r", "0.5", ladderAt); err != nil {
		t.Fatal(err)
	}
	if publisher.count() != 0 {
		t.Fatal("a quote level published an open result")
	}
}

func TestNotifyQuoteOrderReportsWhetherTheLevelMatched(t *testing.T) {
	var seen []bool
	hook := func(_ context.Context, _ store.OrderIntentRecord, _ store.SignedOrderRecord, filled bool) {
		seen = append(seen, filled)
	}
	NotifyQuoteOrder(context.Background(), hook, quoteIntent, store.SignedOrderRecord{State: statemachine.StateLive, MatchedShares: "0"})
	NotifyQuoteOrder(context.Background(), hook, quoteIntent, store.SignedOrderRecord{State: statemachine.StatePartiallyFilled, MatchedShares: "1.5"})
	NotifyQuoteOrder(context.Background(), nil, quoteIntent, store.SignedOrderRecord{MatchedShares: "1"})
	if len(seen) != 2 || seen[0] || !seen[1] {
		t.Fatalf("seen=%v", seen)
	}
}
