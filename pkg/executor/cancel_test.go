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

func TestExecuteCloseLimitCloseEmitsNoImmediateResult(t *testing.T) {
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
	if pub.subject == protocol.SubjectExecutionCloseResult {
		t.Fatalf("expected no immediate close result, got %+v", pub.value)
	}
}

func TestExecuteCloseRejectsMissingPosition(t *testing.T) {
	exec, err := New(&fakeStore{inserted: true}, &fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeForce)); err != nil {
		t.Fatalf("expected missing position to be ignored, got %v", err)
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
	if pub.subject == protocol.SubjectExecutionCloseResult {
		t.Fatalf("expected no immediate force-close result, got %+v", pub.value)
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

func TestExecuteCloseForcePersistsCloseIntentAndInternalChild(t *testing.T) {
	storer := &fakeStore{inserted: true, positions: []store.PositionRecord{{ConditionID: "condition", TokenID: "token", UniqueTag: "lane-a", Outcome: "Up", PositionSize: "2", ActualShares: "2", AvailableSize: "2", State: "open"}}}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeForce)); err != nil {
		t.Fatalf("execute close force: %v", err)
	}
	if len(storer.insertedIntents) != 1 {
		t.Fatalf("expected the force-close intent to be persisted once, got %d: %+v", len(storer.insertedIntents), storer.insertedIntents)
	}
	intent := storer.insertedIntents[0]
	if intent.Kind != store.IntentClose {
		t.Fatalf("expected a CLOSE intent row (orders.intent_id FK), got %+v", intent)
	}
	if storer.order.IntentID != intent.IntentID || storer.order.ChildSequence != 2 {
		t.Fatalf("expected the internal force-close exit child under the persisted intent, got %+v", storer.order)
	}
}

func TestExecuteCloseReplacesActiveCloseWithoutImmediateResult(t *testing.T) {
	storer := &fakeStore{
		inserted:  true,
		positions: []store.PositionRecord{{ConditionID: "condition", TokenID: "token", UniqueTag: "lane-a", Outcome: "Up", PositionSize: "2", ActualShares: "2", AvailableSize: "2", State: "open"}},
		order:     store.SignedOrderRecord{IntentID: "close-old", ChildSequence: 1, ExchangeOrderID: "order-old", State: statemachine.StateLive, Revision: 1},
		intent:    store.OrderIntentRecord{IntentID: "close-old", UniqueTag: "lane-a", ConditionID: "condition", TokenID: "token", Kind: store.IntentClose, Status: statemachine.StateLive},
	}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-new"}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	pub := &resultPublisher{}
	exec.SetEventPublisher(pub)
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeLimit)); err != nil {
		t.Fatalf("execute close replacement: %v", err)
	}
	if client.canceledOrderID != "order-old" {
		t.Fatalf("expected old close order to be canceled, got %q", client.canceledOrderID)
	}
	if storer.intent.Status != store.IntentStatusSuperseded {
		t.Fatalf("expected old close intent to be superseded, got %s", storer.intent.Status)
	}
	if pub.subject == protocol.SubjectExecutionCloseResult {
		t.Fatalf("expected no immediate close result, got %+v", pub.value)
	}
}

func TestExecuteCloseReplacesActiveCloseAlongsidePendingOpenOrder(t *testing.T) {
	storer := &fakeStore{
		inserted: true,
		positions: []store.PositionRecord{
			{ConditionID: "condition", TokenID: "token", UniqueTag: "lane-a", Outcome: "Up", PositionSize: "2", ActualShares: "2", AvailableSize: "2", State: "open"},
		},
		order:  store.SignedOrderRecord{IntentID: "open-1", ChildSequence: 1, ExchangeOrderID: "order-open", State: statemachine.StateCancelRequested, Revision: 1},
		intent: store.OrderIntentRecord{IntentID: "open-1", UniqueTag: "lane-a", ConditionID: "condition", TokenID: "token", Kind: store.IntentOpen},
		extraOrders: []store.SignedOrderRecord{
			{IntentID: "close-old", ChildSequence: 1, ExchangeOrderID: "order-close", State: statemachine.StateLive, Revision: 1},
		},
		extraIntents: map[string]store.OrderIntentRecord{
			"close-old": {IntentID: "close-old", UniqueTag: "lane-a", ConditionID: "condition", TokenID: "token", Kind: store.IntentClose, Status: statemachine.StateLive},
		},
	}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-new"}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	pub := &resultPublisher{}
	exec.SetEventPublisher(pub)
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeLimit)); err != nil {
		t.Fatalf("execute close replacement: %v", err)
	}
	if len(client.canceledOrderIDs) != 2 {
		t.Fatalf("expected both the open and old close orders canceled, got %+v", client.canceledOrderIDs)
	}
	if storer.extraIntents["close-old"].Status != store.IntentStatusSuperseded {
		t.Fatalf("expected old close intent to be superseded, got %s", storer.extraIntents["close-old"].Status)
	}
	if pub.subject == protocol.SubjectExecutionCloseResult {
		t.Fatalf("expected no immediate close result, got %+v", pub.value)
	}
}
