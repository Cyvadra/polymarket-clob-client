package executor

import (
	"context"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

func closeRequest(mode protocol.ExecutionCloseMode) protocol.ExecutionCloseRequest {
	return protocol.ExecutionCloseRequest{SchemaVersion: protocol.SchemaVersionV1, UniqueTag: "lane-a", Strategy: "strategy", ConditionID: "condition", AssetID: "token", Outcome: "Up", Mode: mode, LimitPrice: "0.55", TimeInForce: protocol.TimeInForceGTC}
}

func TestExecuteClosePublishesResult(t *testing.T) {
	storer := &fakeStore{inserted: true, positions: []store.PositionRecord{{ConditionID: "condition", TokenID: "token", UniqueTag: "lane-a", Outcome: "Up", PositionSize: "2", ActualShares: "2", AvailableSize: "2", State: "open"}}}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	pub := &resultPublisher{}
	exec.SetEventPublisher(pub)
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeLimit)); err != nil {
		t.Fatalf("execute close: %v", err)
	}
	if pub.subject != protocol.SubjectExecutionCloseResult {
		t.Fatalf("expected close result subject, got %q", pub.subject)
	}
	if result, ok := pub.value.(protocol.ExecutionCloseResult); !ok || result.Status != protocol.ResultSucceeded {
		t.Fatalf("unexpected close result: %+v", pub.value)
	}
}

func TestExecuteCloseRejectsMissingPosition(t *testing.T) {
	exec, err := New(&fakeStore{inserted: true}, &fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeForce)); err == nil {
		t.Fatal("expected missing position failure")
	}
}

func TestExecuteCloseForceMarksInternalClose(t *testing.T) {
	storer := &fakeStore{inserted: true, positions: []store.PositionRecord{{ConditionID: "condition", TokenID: "token", UniqueTag: "lane-a", Outcome: "Up", PositionSize: "2", ActualShares: "2", AvailableSize: "2", State: "open"}}, order: store.SignedOrderRecord{IntentID: "open-1", ChildSequence: 1, ExchangeOrderID: "order-open", State: statemachine.StateLive, Revision: 1}, intent: store.OrderIntentRecord{IntentID: "open-1", UniqueTag: "lane-a", ConditionID: "condition", TokenID: "token", Kind: store.IntentOpen}}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	pub := &resultPublisher{}
	exec.SetEventPublisher(pub)
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeForce)); err != nil {
		t.Fatalf("execute close force: %v", err)
	}
	if pub.subject != protocol.SubjectExecutionCloseResult {
		t.Fatalf("expected close result subject, got %q", pub.subject)
	}
}

func TestExecuteCloseCancelsOnlyMatchingUniqueTagOpenOrder(t *testing.T) {
	storer := &fakeStore{
		inserted:  true,
		positions: []store.PositionRecord{{ConditionID: "condition", TokenID: "token", UniqueTag: "lane-a", Outcome: "Up", PositionSize: "2", ActualShares: "2", AvailableSize: "2", State: "open"}},
		order:     store.SignedOrderRecord{IntentID: "open-b", ChildSequence: 1, ExchangeOrderID: "order-b", State: statemachine.StateLive, Revision: 1},
		intent:    store.OrderIntentRecord{IntentID: "open-b", UniqueTag: "lane-b", ConditionID: "condition", TokenID: "token", Kind: store.IntentOpen},
	}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "close-order"}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeLimit)); err != nil {
		t.Fatalf("execute close: %v", err)
	}
	if client.canceledOrderID != "" {
		t.Fatalf("unexpected cancellation of different lane order %q", client.canceledOrderID)
	}
}
