package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/mapping"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// freshQuote is a two-sided quote for the fixtures' token, stamped now.
func freshQuote(bid, ask float64) fakeQuotes {
	now := time.Now().UTC()
	return fakeQuotes{has: true, snapshot: marketquotes.Snapshot{
		ConditionID: "condition", At: now,
		Up: marketquotes.Quote{AssetID: "token", Bid: bid, Ask: ask, Mid: (bid + ask) / 2, Timestamp: now},
	}}
}

func driftOpenRequest() protocol.ExecutionOpenRequest {
	req := openRequest()
	req.SignalPrice, req.MaxSignalDrift = "0.5000", "0.0300"
	return req
}

func openResults(pub *recordingPublisher) []protocol.ExecutionOpenResult {
	var results []protocol.ExecutionOpenResult
	for _, record := range pub.publishes {
		if result, ok := record.value.(protocol.ExecutionOpenResult); ok && record.subject == protocol.SubjectExecutionOpenResult {
			results = append(results, result)
		}
	}
	return results
}

// An open whose market has already fallen away from the signal is refused
// before it is signed, since once on the book it could fill at once.
func TestExecuteOpenRefusesAnOpenAlreadyDrifted(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	pub := &recordingPublisher{}
	exec.SetEventPublisher(pub)
	exec.SetQuoteProvider(freshQuote(0.44, 0.46))
	if err := exec.ExecuteOpen(context.Background(), driftOpenRequest()); err == nil {
		t.Fatal("expected the drifted open to be refused")
	}
	if client.submissions != 0 || client.created.TokenID != "" {
		t.Fatalf("a drifted open must not be signed or sent: created=%+v submissions=%d", client.created, client.submissions)
	}
	results := openResults(pub)
	if len(results) != 1 || results[0].Status != protocol.ResultFailed || results[0].ReasonCode != protocol.ReasonSignalDrift {
		t.Fatalf("expected one FAILED SIGNAL_DRIFT result, got %+v", results)
	}
}

// A market that moved in the entry's favour, or not past the bound, is no
// reason to refuse it.
func TestExecuteOpenSubmitsWhenTheMarketMovedItsWay(t *testing.T) {
	for _, quote := range []fakeQuotes{freshQuote(0.60, 0.62), freshQuote(0.48, 0.50)} {
		storer := &fakeStore{inserted: true}
		client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
		exec, err := New(storer, client, time.Now)
		if err != nil {
			t.Fatalf("new executor: %v", err)
		}
		exec.SetQuoteProvider(quote)
		if err := exec.ExecuteOpen(context.Background(), driftOpenRequest()); err != nil {
			t.Fatalf("execute open at %+v: %v", quote.snapshot.Up, err)
		}
		if client.submissions != 1 || storer.insertedIntents[0].SignalPrice != "0.5000" {
			t.Fatalf("expected the open submitted with its bound stored: submissions=%d intents=%+v", client.submissions, storer.insertedIntents)
		}
	}
}

func TestExecuteOpenRefusesADriftBoundOnAnImmediateOrder(t *testing.T) {
	exec, err := New(&fakeStore{inserted: true}, &fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	pub := &recordingPublisher{}
	exec.SetEventPublisher(pub)
	req := driftOpenRequest()
	req.TimeInForce = protocol.TimeInForceFAK
	if err := exec.ExecuteOpen(context.Background(), req); err == nil {
		t.Fatal("expected a FAK with a drift bound to be refused")
	}
	if results := openResults(pub); len(results) != 1 || results[0].ReasonCode != protocol.ReasonInvalidIntent {
		t.Fatalf("expected INVALID_INTENT, got %+v", results)
	}
}

func TestExecuteOpenRefusesARestWindowOnAnImmediateOrder(t *testing.T) {
	exec, err := New(&fakeStore{inserted: true}, &fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	pub := &recordingPublisher{}
	exec.SetEventPublisher(pub)
	req := driftOpenRequest()
	req.SignalPrice, req.MaxSignalDrift = "", ""
	req.TimeInForce = protocol.TimeInForceFAK
	req.Policy.RestAfterAckMillis = 350
	if err := exec.ExecuteOpen(context.Background(), req); err == nil {
		t.Fatal("expected a FAK with rest_after_ack_ms to be refused")
	}
	if results := openResults(pub); len(results) != 1 || results[0].ReasonCode != protocol.ReasonInvalidIntent {
		t.Fatalf("expected INVALID_INTENT, got %+v", results)
	}
}

func restingDriftOpen() *fakeStore {
	now := time.Now().UTC()
	return &fakeStore{
		inserted: true,
		order: store.SignedOrderRecord{
			IntentID: "open-1", ChildSequence: store.StrategyChildSequence, ExchangeOrderID: "order-open",
			State: statemachine.StateLive, Revision: 2, RequestedShares: "20", MatchedShares: "0",
		},
		intent: store.OrderIntentRecord{
			IntentID: "open-1", UniqueTag: "lane-a", Strategy: "strategy", ConditionID: "condition",
			TokenID: "token", Outcome: "Up", Kind: store.IntentOpen, Side: "BUY",
			LimitPrice: "0.53", TimeInForce: "GTC", Status: statemachine.StateLive,
			CreatedAt: now, ExpiresAt: now.Add(3 * time.Second),
			SignalPrice: "0.500000000000000000", MaxSignalDrift: "0.030000000000000000",
		},
	}
}

// watchingExecutor is an executor watching storer's resting order, with the
// given quote and clock.
func watchingExecutor(t *testing.T, storer *fakeStore, client *fakeCLOB, quotes fakeQuotes, now func() time.Time) *Executor {
	t.Helper()
	exec, err := New(storer, client, now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	exec.SetEventPublisher(&recordingPublisher{})
	exec.SetQuoteProvider(quotes)
	exec.watchOrder(mapping.ExecutionIntent(storer.intent), storer.order.ChildSequence, storer.intent.CreatedAt, time.Time{})
	return exec
}

func watching(exec *Executor) int {
	exec.watchMu.Lock()
	defer exec.watchMu.Unlock()
	return len(exec.watched)
}

// A resting open is cancelled by the watcher once the market falls away from
// its signal, its intent marked so the result names it, and it is dropped
// once the store shows the cancel accepted.
func TestCheckWatchedCancelsARestingOpenThatDrifted(t *testing.T) {
	storer := restingDriftOpen()
	client := &fakeCLOB{}
	exec := watchingExecutor(t, storer, client, freshQuote(0.43, 0.45), time.Now)
	if err := exec.checkWatched(context.Background()); err != nil {
		t.Fatalf("watch pass: %v", err)
	}
	if client.canceledOrderID != "order-open" || storer.order.State != statemachine.StateCancelPending {
		t.Fatalf("expected the drifted order cancelled, got %q in %q", client.canceledOrderID, storer.order.State)
	}
	if storer.intent.Status != store.IntentStatusSignalDrift {
		t.Fatalf("expected the intent marked SIGNAL_DRIFT, got %q", storer.intent.Status)
	}
	if err := exec.checkWatched(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(client.canceledOrderIDs) != 1 || watching(exec) != 0 {
		t.Fatalf("expected one cancel then unwatched: cancels=%v watching=%d", client.canceledOrderIDs, watching(exec))
	}
}

// The watcher reads only quotes and the clock while nothing is due.
func TestCheckWatchedLeavesARestingOpenThatMovedItsWay(t *testing.T) {
	storer := restingDriftOpen()
	client := &fakeCLOB{}
	exec := watchingExecutor(t, storer, client, freshQuote(0.70, 0.72), time.Now)
	if err := exec.checkWatched(context.Background()); err != nil {
		t.Fatalf("watch pass: %v", err)
	}
	if client.canceledOrderID != "" || storer.intent.Status != statemachine.StateLive {
		t.Fatalf("a favourable move must not cancel: canceled=%q status=%q", client.canceledOrderID, storer.intent.Status)
	}
	if watching(exec) != 1 {
		t.Fatal("expected the open still watched")
	}
}

// An order past its deadline is cancelled with a deadline mark, not a drift
// mark, so its result says EXPIRED.
func TestCheckWatchedCancelsAnExpiredOrder(t *testing.T) {
	storer := restingDriftOpen()
	client := &fakeCLOB{}
	now := time.Now().UTC()
	exec := watchingExecutor(t, storer, client, freshQuote(0.70, 0.72), func() time.Time { return now })
	now = storer.intent.ExpiresAt
	if err := exec.checkWatched(context.Background()); err != nil {
		t.Fatalf("watch pass: %v", err)
	}
	if client.canceledOrderID != "order-open" || storer.intent.Status != store.IntentStatusDeadline {
		t.Fatalf("expected a deadline cancel: canceled=%q status=%q", client.canceledOrderID, storer.intent.Status)
	}
}

// rest_after_ack_ms runs from the ack, so a slow submission keeps its whole
// window; expires_at still caps it.
func TestWatchOrderRestsAfterTheAck(t *testing.T) {
	storer := restingDriftOpen()
	storer.intent.MaxSignalDrift = ""
	created := storer.intent.CreatedAt
	exec, err := New(storer, &fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	intent := mapping.ExecutionIntent(storer.intent)
	deadline := func(ack time.Time, rest time.Duration, expires time.Time) time.Time {
		exec.watched = nil
		intent.Policy.RestAfterAckMillis = rest.Milliseconds()
		intent.ExpiresAt = expires
		exec.watchOrder(intent, storer.order.ChildSequence, created, ack)
		return exec.watched[orderKey{intent.IntentID, storer.order.ChildSequence}].deadline
	}
	ack := created.Add(700 * time.Millisecond)
	if got, want := deadline(ack, 350*time.Millisecond, created.Add(2*time.Second)), ack.Add(350*time.Millisecond); !got.Equal(want) {
		t.Fatalf("a slow ack keeps its window: deadline %v, want %v", got, want)
	}
	if got, want := deadline(ack, 3*time.Second, created.Add(2*time.Second)), created.Add(2*time.Second); !got.Equal(want) {
		t.Fatalf("expires_at caps the window: deadline %v, want %v", got, want)
	}
	if got, want := deadline(time.Time{}, 350*time.Millisecond, created.Add(2*time.Second)), created.Add(2*time.Second); !got.Equal(want) {
		t.Fatalf("an adopted order without an ack keeps the cap: deadline %v, want %v", got, want)
	}
}

// A cancel the exchange refuses is re-sent every pass until it lands.
func TestCheckWatchedRetriesAFailedCancel(t *testing.T) {
	storer := restingDriftOpen()
	client := &fakeCLOB{cancelErrs: []error{errors.New("timeout")}}
	now := time.Now().UTC()
	exec := watchingExecutor(t, storer, client, freshQuote(0.43, 0.45), func() time.Time { return now })
	if err := exec.checkWatched(context.Background()); err != nil {
		t.Fatalf("a failed attempt is retried, not reported: %v", err)
	}
	if storer.order.State != statemachine.StateCancelRequested {
		t.Fatalf("expected the order left CANCEL_REQUESTED, got %q", storer.order.State)
	}
	now = now.Add(watchInterval)
	if err := exec.checkWatched(context.Background()); err != nil {
		t.Fatalf("retry pass: %v", err)
	}
	if len(client.canceledOrderIDs) != 2 || storer.order.State != statemachine.StateCancelPending {
		t.Fatalf("expected the cancel re-sent and accepted: cancels=%v state=%q", client.canceledOrderIDs, storer.order.State)
	}
}

// A cancel that fails because the order filled stops once the fill lands.
func TestCheckWatchedStopsOnceTheOrderFilled(t *testing.T) {
	storer := restingDriftOpen()
	client := &fakeCLOB{cancelErrs: []error{errors.New("order already matched")}}
	now := time.Now().UTC()
	exec := watchingExecutor(t, storer, client, freshQuote(0.43, 0.45), func() time.Time { return now })
	_ = exec.checkWatched(context.Background())
	storer.order.State = statemachine.StateFilled
	now = now.Add(watchInterval)
	if err := exec.checkWatched(context.Background()); err != nil {
		t.Fatalf("watch pass: %v", err)
	}
	if len(client.canceledOrderIDs) != 1 || watching(exec) != 0 {
		t.Fatalf("expected no cancel after the fill: cancels=%v watching=%d", client.canceledOrderIDs, watching(exec))
	}
}

// After cancelRetryWindow of refused cancels the watcher gives up and reports
// it, and is not restarted by the recovery pass while the order stays open.
func TestCheckWatchedGivesUpAfterTheRetryWindow(t *testing.T) {
	storer := restingDriftOpen()
	storer.intent.ExpiresAt = time.Now().Add(time.Minute)
	refused := make([]error, 100)
	for i := range refused {
		refused[i] = errors.New("timeout")
	}
	client := &fakeCLOB{cancelErrs: refused}
	now := time.Now().UTC()
	exec := watchingExecutor(t, storer, client, freshQuote(0.43, 0.45), func() time.Time { return now })
	for elapsed := time.Duration(0); elapsed < cancelRetryWindow; elapsed += watchInterval {
		if err := exec.checkWatched(context.Background()); err != nil {
			t.Fatalf("retry at %s reported early: %v", elapsed, err)
		}
		now = now.Add(watchInterval)
	}
	if err := exec.checkWatched(context.Background()); err == nil {
		t.Fatal("expected giving up reported")
	}
	attempts := int(cancelRetryWindow / watchInterval)
	if err := exec.recoverOpenOrders(context.Background()); err != nil {
		t.Fatalf("recovery pass: %v", err)
	}
	now = now.Add(watchInterval)
	_ = exec.checkWatched(context.Background())
	if got := len(client.canceledOrderIDs); got != attempts {
		t.Fatalf("expected %d attempts and no more: got %d", attempts, got)
	}
	storer.order = store.SignedOrderRecord{} // no longer in the open set
	if err := exec.recoverOpenOrders(context.Background()); err != nil {
		t.Fatalf("recovery pass: %v", err)
	}
	if watching(exec) != 0 {
		t.Fatal("expected the order dropped once no longer open")
	}
}

// An order another path already asked to cancel is still cancelled when due,
// but not marked as drifted.
func TestCheckWatchedDoesNotMarkACancelItDidNotStart(t *testing.T) {
	storer := restingDriftOpen()
	storer.order.State = statemachine.StateCancelRequested
	client := &fakeCLOB{}
	exec := watchingExecutor(t, storer, client, freshQuote(0.43, 0.45), time.Now)
	if err := exec.checkWatched(context.Background()); err != nil {
		t.Fatalf("watch pass: %v", err)
	}
	if client.canceledOrderID != "order-open" || storer.intent.Status == store.IntentStatusSignalDrift {
		t.Fatalf("expected a cancel without the mark: canceled=%q status=%q", client.canceledOrderID, storer.intent.Status)
	}
}

// A resting order open before a restart is adopted by the recovery pass.
func TestRecoverOpenOrdersAdoptsARestingOrder(t *testing.T) {
	storer := restingDriftOpen()
	exec, err := New(storer, &fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	if err := exec.recoverOpenOrders(context.Background()); err != nil {
		t.Fatalf("recovery pass: %v", err)
	}
	if watching(exec) != 1 {
		t.Fatal("expected the resting order watched")
	}
}

// An order that filled between the watcher's read and its drift mark is not
// marked, and its cancel is not sent; the next pass drops it.
func TestCheckWatchedDoesNotMarkAnOrderThatJustFilled(t *testing.T) {
	storer := restingDriftOpen()
	client := &fakeCLOB{}
	exec := watchingExecutor(t, storer, client, freshQuote(0.43, 0.45), time.Now)
	filled := &fillOnReadStore{fakeStore: storer}
	exec.store = filled
	if err := exec.checkWatched(context.Background()); err != nil {
		t.Fatalf("watch pass: %v", err)
	}
	if client.canceledOrderID != "" || storer.intent.Status == store.IntentStatusSignalDrift {
		t.Fatalf("expected no mark and no cancel: canceled=%q status=%q", client.canceledOrderID, storer.intent.Status)
	}
	if err := exec.checkWatched(context.Background()); err != nil || watching(exec) != 0 {
		t.Fatalf("expected the filled order dropped: err=%v watching=%d", err, watching(exec))
	}
}

// fillOnReadStore fills the order just after the watcher reads it, as the
// account feed would between the read and the drift mark.
type fillOnReadStore struct{ *fakeStore }

func (s *fillOnReadStore) OrderByIntent(ctx context.Context, intentID string, childSequence int) (store.SignedOrderRecord, error) {
	order, err := s.fakeStore.OrderByIntent(ctx, intentID, childSequence)
	s.fakeStore.order.State = statemachine.StateFilled
	return order, err
}

// An order the user stream reports live before the submission response lands
// is still watched, and its rest window runs from that observation, not from
// expires_at: 28 of 37 FairDelta cancels on 2026-10-18 rested the whole
// 1.3s to expires_at for want of this.
func TestSendOrderWatchesAnOrderTheStreamAcknowledgedFirst(t *testing.T) {
	storer := restingDriftOpen()
	storer.intent.MaxSignalDrift = ""
	created := storer.intent.CreatedAt
	observed := created.Add(120 * time.Millisecond)
	now := created.Add(400 * time.Millisecond)
	storer.streamObservedAt = observed
	storer.order = store.SignedOrderRecord{IntentID: "open-1", ChildSequence: store.StrategyChildSequence, State: statemachine.StateSigned, Revision: 1, MatchedShares: "0", RequestedShares: "20"}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-open", Status: "live"}}
	exec, err := New(storer, client, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	exec.SetEventPublisher(&recordingPublisher{})
	intent := mapping.ExecutionIntent(storer.intent)
	intent.Policy.RestAfterAckMillis = 350
	intent.ExpiresAt = created.Add(2 * time.Second)
	ctx := context.WithValue(context.Background(), intentCreatedAtKey{}, created)
	child := plannedChild{Sequence: store.StrategyChildSequence, Shares: "20", Price: "0.53", TimeInForce: protocol.TimeInForceGTC}
	if err := exec.submitOrder(ctx, intent, clobclient.SignedOrderV2{OrderID: "order-open"}, child, 1); err != nil {
		t.Fatalf("submit: %v", err)
	}
	watched, ok := exec.watched[orderKey{"open-1", store.StrategyChildSequence}]
	if !ok {
		t.Fatal("expected the order watched after the stream's acknowledgement")
	}
	if want := observed.Add(350 * time.Millisecond); !watched.deadline.Equal(want) {
		t.Fatalf("deadline %v, want the rest window from the stream's observation %v", watched.deadline, want)
	}
}

// The drift check reads the maintained book before the quote stream: a print
// away from the book moves the quote's mid, and refused FairDelta entries on
// 2026-10-18 at a mid of 0.58 against a book of 0.72/0.73. A stale book
// hands back to the quote.
func TestDriftReadsTheBookBeforeTheQuote(t *testing.T) {
	storer := restingDriftOpen()
	storer.intent.SignalPrice = "0.725"
	client := &fakeCLOB{}
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	exec := watchingExecutor(t, storer, client, freshQuote(0.57, 0.59), clock)
	book := freshQuote(0.72, 0.73)
	exec.SetBookProvider(book)
	if err := exec.checkWatched(context.Background()); err != nil {
		t.Fatalf("watch pass: %v", err)
	}
	if client.canceledOrderID != "" {
		t.Fatal("a print off the book must not cancel an open the book never moved from")
	}
	now = now.Add(3 * time.Second)
	exec.SetQuoteProvider(freshQuote(0.57, 0.59))
	if err := exec.checkWatched(context.Background()); err != nil {
		t.Fatalf("watch pass: %v", err)
	}
	if client.canceledOrderID != "order-open" {
		t.Fatal("with the book stale, the fresh quote decides")
	}
}

// The pre-submission check reads the same book.
func TestExecuteOpenSubmitsWhenOnlyAPrintMovedAgainstIt(t *testing.T) {
	storer := &fakeStore{inserted: true}
	client := &fakeCLOB{response: &clobclient.OrderResponse{Success: true, OrderID: "order-1"}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	exec.SetQuoteProvider(freshQuote(0.40, 0.42))
	exec.SetBookProvider(freshQuote(0.49, 0.51))
	if err := exec.ExecuteOpen(context.Background(), driftOpenRequest()); err != nil {
		t.Fatalf("execute open: %v", err)
	}
	if client.submissions != 1 {
		t.Fatalf("expected the open submitted on the book's mid, submissions=%d", client.submissions)
	}
}
