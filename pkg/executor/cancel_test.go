package executor

import (
	"context"
	"strings"
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

func lanePosition() []store.PositionRecord {
	return []store.PositionRecord{{ConditionID: "condition", TokenID: "token", UniqueTag: "lane-a", Outcome: "Up", PositionSize: "2", ActualShares: "2", AvailableSize: "2", State: "open"}}
}

func unsettledBalanceError() error {
	return &clobclient.APIError{StatusCode: 400, Method: "POST", Path: "/order", Body: []byte(`{"error":"not enough balance / allowance: the balance is not enough -> balance: 0, order amount: 2000000"}`)}
}

func closeResults(pub *recordingPublisher) []protocol.ExecutionCloseResult {
	var results []protocol.ExecutionCloseResult
	for _, record := range pub.publishes {
		if result, ok := record.value.(protocol.ExecutionCloseResult); ok && record.subject == protocol.SubjectExecutionCloseResult {
			results = append(results, result)
		}
	}
	return results
}

func TestExecuteCloseLimitRejectedPublishesFailedResult(t *testing.T) {
	storer := &fakeStore{inserted: true, positions: lanePosition()}
	client := &fakeCLOB{submitErr: &clobclient.APIError{StatusCode: 400, Method: "POST", Path: "/order", Body: []byte(`{"error":"invalid order"}`)}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	pub := &recordingPublisher{}
	exec.SetEventPublisher(pub)
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeLimit)); err == nil {
		t.Fatal("expected the exchange rejection to be returned")
	}
	results := closeResults(pub)
	if len(results) != 1 || results[0].Status != protocol.ResultFailed || results[0].ReasonCode != protocol.ReasonOrderRejected || results[0].UniqueTag != "lane-a" || results[0].AssetID != "token" {
		t.Fatalf("expected one FAILED ORDER_REJECTED close result for the lane, got %+v", results)
	}
	if client.submissions != 1 {
		t.Fatalf("expected a non-balance rejection not to be retried, got %d submissions", client.submissions)
	}
}

func TestExecuteCloseRetriesUnsettledBalanceWithFreshIntent(t *testing.T) {
	previous := settlementRetryInterval
	settlementRetryInterval = time.Millisecond
	t.Cleanup(func() { settlementRetryInterval = previous })
	storer := &fakeStore{inserted: true, positions: lanePosition()}
	client := &fakeCLOB{submitErrs: []error{unsettledBalanceError()}, response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	pub := &recordingPublisher{}
	exec.SetEventPublisher(pub)
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeLimit)); err != nil {
		t.Fatalf("expected the retried close to be placed, got %v", err)
	}
	if client.submissions != 2 {
		t.Fatalf("expected one retry after the balance rejection, got %d submissions", client.submissions)
	}
	if len(storer.insertedIntents) != 2 || storer.insertedIntents[0].IntentID == storer.insertedIntents[1].IntentID {
		t.Fatalf("expected the retry under a fresh intent ID, got %+v", storer.insertedIntents)
	}
	if results := closeResults(pub); len(results) != 0 {
		t.Fatalf("expected no close result while the retried close rests, got %+v", results)
	}
}

func TestExecuteCloseUnsettledBalanceGivesUpWithFailedResult(t *testing.T) {
	previous := settlementRetryInterval
	settlementRetryInterval = time.Millisecond
	t.Cleanup(func() { settlementRetryInterval = previous })
	// Each clock read advances 10s, so the settlement window passes after
	// the first retry decision.
	start, reads := time.Now(), 0
	clock := func() time.Time {
		reads++
		return start.Add(time.Duration(reads) * 10 * time.Second)
	}
	storer := &fakeStore{inserted: true, positions: lanePosition()}
	client := &fakeCLOB{submitErr: unsettledBalanceError()}
	exec, err := New(storer, client, clock)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	pub := &recordingPublisher{}
	exec.SetEventPublisher(pub)
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeLimit)); err == nil {
		t.Fatal("expected the close to give up once the settlement window passed")
	}
	results := closeResults(pub)
	if len(results) != 1 || results[0].Status != protocol.ResultFailed || results[0].ReasonCode != protocol.ReasonOrderRejected || !strings.Contains(results[0].Reason, "not enough balance") {
		t.Fatalf("expected one FAILED ORDER_REJECTED result carrying the exchange reason, got %+v", results)
	}
}

func TestExecuteCloseForceRetriesUnsettledBalanceWithoutResult(t *testing.T) {
	previous := settlementRetryInterval
	settlementRetryInterval = time.Millisecond
	t.Cleanup(func() { settlementRetryInterval = previous })
	storer := &fakeStore{inserted: true, positions: lanePosition()}
	client := &fakeCLOB{submitErrs: []error{unsettledBalanceError()}, response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	pub := &recordingPublisher{}
	exec.SetEventPublisher(pub)
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeForce)); err != nil {
		t.Fatalf("expected the retried force close to be placed, got %v", err)
	}
	if client.submissions != 2 {
		t.Fatalf("expected one retry after the balance rejection, got %d submissions", client.submissions)
	}
	if results := closeResults(pub); len(results) != 0 {
		t.Fatalf("expected FORCE_CLOSE never to publish a close result, got %+v", results)
	}
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
