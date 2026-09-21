package executor

import (
	"context"
	"strings"
	"sync"
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

// laneWithOpenAndClose models a lane carrying both a resting open buy and a
// prior close, the state a replacement close has to resolve.
func laneWithOpenAndClose() *fakeStore {
	return &fakeStore{
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
}

// A LIMIT_CLOSE supersedes a prior close but leaves a resting open buy working,
// so a partially filled entry keeps growing; the maintenance pass resizes the
// close to match. Only a FORCE_CLOSE freezes the lane.
func TestExecuteCloseLimitReplacesCloseButLeavesPendingOpenOrder(t *testing.T) {
	storer := laneWithOpenAndClose()
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
	if len(client.canceledOrderIDs) != 1 || client.canceledOrderIDs[0] != "order-close" {
		t.Fatalf("expected only the old close canceled, got %+v", client.canceledOrderIDs)
	}
	if storer.extraIntents["close-old"].Status != store.IntentStatusSuperseded {
		t.Fatalf("expected old close intent to be superseded, got %s", storer.extraIntents["close-old"].Status)
	}
	if pub.subject == protocol.SubjectExecutionCloseResult {
		t.Fatalf("expected no immediate close result, got %+v", pub.value)
	}
}

// A FORCE_CLOSE retires every child on the lane, the open buy included: it
// exits before settlement and needs the position to stop moving.
func TestExecuteCloseForceCancelsPendingOpenOrderToo(t *testing.T) {
	storer := laneWithOpenAndClose()
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-new"}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	exec.SetEventPublisher(&resultPublisher{})
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeForce)); err != nil {
		t.Fatalf("execute force close: %v", err)
	}
	if len(client.canceledOrderIDs) != 2 {
		t.Fatalf("expected both the open and old close orders canceled, got %+v", client.canceledOrderIDs)
	}
}

// A close is sized from what can actually be reserved, not from the lane's
// gross holding: the store gates the sell reservation on available_size, so
// planning from actual_shares places an order the reservation would refuse.
func TestExecuteCloseSizesFromTheReservableShares(t *testing.T) {
	storer := &fakeStore{
		inserted: true,
		positions: []store.PositionRecord{{
			ConditionID: "condition", TokenID: "token", UniqueTag: "lane-a", Outcome: "Up",
			PositionSize: "10", ActualShares: "10", AvailableSize: "4", ReservedSize: "6", State: "open",
		}},
	}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-new"}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	exec.SetEventPublisher(&resultPublisher{})
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeLimit)); err != nil {
		t.Fatalf("execute close: %v", err)
	}
	if client.created.Shares != 4 {
		t.Fatalf("expected the close sized at the 4 reservable shares, got %v", client.created.Shares)
	}
}

// A refusal that names the exchange's own holding resizes the lane to it, so
// the retry plans from the wallet's truth rather than the local fill ledger.
func TestExecuteCloseAdoptsTheExchangeReportedPosition(t *testing.T) {
	previous := settlementRetryInterval
	settlementRetryInterval = time.Millisecond
	t.Cleanup(func() { settlementRetryInterval = previous })

	storer := &fakeStore{
		inserted: true,
		positions: []store.PositionRecord{{
			ConditionID: "condition", TokenID: "token", UniqueTag: "lane-a", Outcome: "Up",
			PositionSize: "10", ActualShares: "10", AvailableSize: "10", State: "open",
		}},
	}
	shortBalance := &clobclient.APIError{StatusCode: 400, Method: "POST", Path: "/order",
		Body: []byte(`{"error":"not enough balance / allowance: the balance is not enough -> balance: 4000000, order amount: 10000000"}`)}
	client := &fakeCLOB{
		submitErrs: []error{shortBalance},
		response:   &clobclient.OrderResponse{Success: true, OrderID: "order-new"},
	}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	exec.SetEventPublisher(&recordingPublisher{})
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeLimit)); err != nil {
		t.Fatalf("execute close: %v", err)
	}
	if len(storer.reconciled) != 1 || storer.reconciled[0].shares != "4" {
		t.Fatalf("expected the lane clamped to the reported 4 shares, got %+v", storer.reconciled)
	}
	if client.created.Shares != 4 {
		t.Fatalf("expected the retry sized at the exchange's 4 shares, got %v", client.created.Shares)
	}
}

// A reported balance of zero is the unsettled case, not a smaller holding: the
// fill is booked but the tokens have not reached the wallet. Clamping there
// would read as "nothing left to exit" and cancel the settlement retry.
func TestExecuteCloseDoesNotAdoptAZeroReportedBalance(t *testing.T) {
	previous := settlementRetryInterval
	settlementRetryInterval = time.Millisecond
	t.Cleanup(func() { settlementRetryInterval = previous })

	storer := &fakeStore{inserted: true, positions: lanePosition()}
	client := &fakeCLOB{
		submitErrs: []error{unsettledBalanceError()},
		response:   &clobclient.OrderResponse{Success: true, OrderID: "order-new"},
	}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	exec.SetEventPublisher(&recordingPublisher{})
	if err := exec.ExecuteClose(context.Background(), closeRequest(protocol.ExecutionCloseModeLimit)); err != nil {
		t.Fatalf("execute close: %v", err)
	}
	if len(storer.reconciled) != 0 {
		t.Fatalf("expected no clamp from a zero balance, got %+v", storer.reconciled)
	}
	if client.created.Shares != 2 {
		t.Fatalf("expected the retry to keep the full position, got %v", client.created.Shares)
	}
}

// lockedStore serialises the handful of reads and writes the two Run passes
// make concurrently. The plain fakeStore is written for single-goroutine
// tests; only this one exercises Run itself.
type lockedStore struct {
	*fakeStore
	mu sync.Mutex
}

func (s *lockedStore) OpenOrders(ctx context.Context) ([]store.SignedOrderRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.OpenOrders(ctx)
}
func (s *lockedStore) Intent(ctx context.Context, intentID string) (store.OrderIntentRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.Intent(ctx, intentID)
}
func (s *lockedStore) PositionFeatures(ctx context.Context) ([]store.PositionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.PositionFeatures(ctx)
}
func (s *lockedStore) TransitionOrder(ctx context.Context, order store.SignedOrderRecord, event statemachine.Event, matchedShares, exchangeOrderID, reason string) (store.SignedOrderRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.TransitionOrder(ctx, order, event, matchedShares, exchangeOrderID, reason)
}

// blockingCLOB holds every MinOrderSize call until the test releases it,
// standing in for the production case: one exchange round trip per lane, with
// enough settled lanes to make a maintenance pass take a minute.
type blockingCLOB struct {
	*fakeCLOB
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	cancels []string
}

func (c *blockingCLOB) MinOrderSize(context.Context, string) (float64, error) {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	<-c.release
	return 0, nil
}
func (c *blockingCLOB) CancelOrder(_ context.Context, orderID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancels = append(c.cancels, orderID)
	return nil
}
func (c *blockingCLOB) canceled() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.cancels...)
}

// An order past its expires_at is cancelled even while a close-maintenance
// pass is stuck on the exchange. The two passes shared a goroutine until
// 2026-09-22, which let one slow pass stretch the deadline check from 1s to
// 60s and leave orders resting long past their deadline.
func TestRunCancelsExpiredOrdersWhileCloseMaintenanceIsBlocked(t *testing.T) {
	// The deadline falls after the first tick, so the cancel can only be made
	// by a later pass — the one a blocked maintenance pass used to swallow.
	start := time.Now().UTC()
	expires := start.Add(2 * time.Second)
	storer := &lockedStore{fakeStore: &fakeStore{
		inserted:  true,
		positions: lanePosition(),
		order: store.SignedOrderRecord{
			IntentID: "open-1", ChildSequence: 1, ExchangeOrderID: "order-open",
			State: statemachine.StateLive, Revision: 1, RequestedShares: "2", MatchedShares: "0",
		},
		intent: store.OrderIntentRecord{
			IntentID: "open-1", UniqueTag: "lane-a", Strategy: "strategy", ConditionID: "condition",
			TokenID: "token", Outcome: "Up", Kind: store.IntentOpen, Side: "BUY",
			LimitPrice: "0.88", TimeInForce: "GTC", Status: statemachine.StateLive,
			CreatedAt: start, ExpiresAt: expires,
		},
	}}
	client := &blockingCLOB{
		fakeCLOB: &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-new"}},
		entered:  make(chan struct{}, 1),
		release:  make(chan struct{}),
	}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	exec.SetEventPublisher(&recordingPublisher{})
	exec.SetQuoteProvider(quotesAtBid(0.55))

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = exec.Run(ctx) }()
	defer func() {
		stop()
		close(client.release)
		<-done
	}()

	// Wait for close maintenance to be wedged in the exchange call before
	// judging the deadline pass, so the test cannot pass by racing it.
	select {
	case <-client.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("close maintenance never reached the exchange")
	}
	deadline := expires.Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(client.canceled()) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expired order was not cancelled while close maintenance was blocked")
}
