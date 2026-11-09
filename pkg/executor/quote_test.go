package executor

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/mapping"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// quoteSink records the quote results executiond publishes.
type quoteSink struct {
	mu      sync.Mutex
	results []protocol.ExecutionQuoteResult
	fail    error
}

func (p *quoteSink) PublishJSON(subject string, value any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if result, ok := value.(protocol.ExecutionQuoteResult); ok && subject == protocol.SubjectExecutionQuoteResult {
		if p.fail != nil {
			return p.fail
		}
		p.results = append(p.results, result)
	}
	return nil
}

func (p *quoteSink) published() []protocol.ExecutionQuoteResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.results)
}

type quoteHarness struct {
	*ladderHarness
	quotes *quoteSink
}

func newQuoteHarness(t *testing.T) *quoteHarness {
	t.Helper()
	h := &quoteHarness{ladderHarness: newLadderHarness(t), quotes: &quoteSink{}}
	h.exec.SetEventPublisher(h.quotes)
	return h
}

var quoteKey = store.QuoteLaneKey{UniqueTag: "lane", ConditionID: "cond", TokenID: "token"}

func quoteRequest(seq int64, levels ...string) protocol.ExecutionQuoteRequest {
	req := protocol.ExecutionQuoteRequest{
		SchemaVersion: protocol.SchemaVersionV1, RequestID: "req-" + string(rune('a'+seq%26)), UniqueTag: "lane", Strategy: "strategy",
		ConditionID: "cond", TokenID: "token", Outcome: "Up", Side: protocol.SideBuy, Seq: seq,
		Levels: levels, PostOnly: true, ExpiresAt: time.Now().Add(3 * time.Second),
	}
	if len(levels) > 0 {
		req.TargetUSD = "5"
	}
	return req
}

// apply sends a request and waits until its lane has applied everything.
func (h *quoteHarness) apply(t *testing.T, req protocol.ExecutionQuoteRequest) {
	t.Helper()
	if err := h.exec.ApplyQuote(context.Background(), req); err != nil {
		t.Fatalf("apply quote: %v", err)
	}
	h.idle(t)
}

func (h *quoteHarness) idle(t *testing.T) {
	t.Helper()
	lane := h.exec.quoteLanes.lane(quoteKey)
	eventually(t, "quote lane idle", func() bool {
		if !lane.ops.TryLock() {
			return false
		}
		defer lane.ops.Unlock()
		lane.mu.Lock()
		defer lane.mu.Unlock()
		return !lane.draining && lane.pending == nil
	})
}

func (h *quoteHarness) lane() *quoteLane { return h.exec.quoteLanes.lane(quoteKey) }

// working is the lane's working levels, in cents.
func (h *quoteHarness) working() []int {
	lane := h.lane()
	lane.mu.Lock()
	defer lane.mu.Unlock()
	var cents []int
	for c := range lane.levels {
		cents = append(cents, c)
	}
	slices.Sort(cents)
	return cents
}

func (h *quoteHarness) orderAt(t *testing.T, cents int) store.SignedOrderRecord {
	t.Helper()
	lane := h.lane()
	lane.mu.Lock()
	level := lane.levels[cents]
	intentID := lane.intent.IntentID
	lane.mu.Unlock()
	if level == nil {
		t.Fatalf("no working level at %d cents", cents)
	}
	order, err := h.store.OrderByIntent(context.Background(), intentID, level.sequence)
	if err != nil {
		t.Fatal(err)
	}
	return order
}

func (h *quoteHarness) cancels() []string {
	h.clob.mu.Lock()
	defer h.clob.mu.Unlock()
	return slices.Clone(h.clob.cancels)
}

func TestValidateQuote(t *testing.T) {
	for name, tc := range map[string]struct {
		edit    func(*protocol.ExecutionQuoteRequest)
		wantErr bool
	}{
		"valid":            {edit: func(*protocol.ExecutionQuoteRequest) {}},
		"empty levels":     {edit: func(r *protocol.ExecutionQuoteRequest) { r.Levels, r.TargetUSD = nil, "" }},
		"sell":             {edit: func(r *protocol.ExecutionQuoteRequest) { r.Side = protocol.SideSell }, wantErr: true},
		"not post-only":    {edit: func(r *protocol.ExecutionQuoteRequest) { r.PostOnly = false }, wantErr: true},
		"off cent":         {edit: func(r *protocol.ExecutionQuoteRequest) { r.Levels = []string{"0.525"} }, wantErr: true},
		"one":              {edit: func(r *protocol.ExecutionQuoteRequest) { r.Levels = []string{"1.00"} }, wantErr: true},
		"ascending":        {edit: func(r *protocol.ExecutionQuoteRequest) { r.Levels = []string{"0.50", "0.51"} }, wantErr: true},
		"duplicate":        {edit: func(r *protocol.ExecutionQuoteRequest) { r.Levels = []string{"0.51", "0.51"} }, wantErr: true},
		"no size":          {edit: func(r *protocol.ExecutionQuoteRequest) { r.TargetUSD = "" }, wantErr: true},
		"both sizes":       {edit: func(r *protocol.ExecutionQuoteRequest) { r.TargetEquityFraction = "0.01" }, wantErr: true},
		"no expiry":        {edit: func(r *protocol.ExecutionQuoteRequest) { r.ExpiresAt = time.Time{} }, wantErr: true},
		"half drift bound": {edit: func(r *protocol.ExecutionQuoteRequest) { r.SignalPrice = "0.53" }, wantErr: true},
		"too many levels": {edit: func(r *protocol.ExecutionQuoteRequest) {
			r.Levels = nil
			for c := 90; c > 60; c-- {
				r.Levels = append(r.Levels, centsPrice(c))
			}
		}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			req := quoteRequest(1, "0.52", "0.51", "0.50")
			tc.edit(&req)
			_, err := validateQuote(req, DefaultMaxLadderLevels)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestQuoteRequoteKeepsWantedLevelsWithoutExchangeCalls(t *testing.T) {
	h := newQuoteHarness(t)
	h.apply(t, quoteRequest(1, "0.52", "0.51", "0.50"))
	if got := h.working(); !slices.Equal(got, []int{50, 51, 52}) {
		t.Fatalf("working=%v", got)
	}
	kept52, kept51, dropped := h.orderAt(t, 52), h.orderAt(t, 51), h.orderAt(t, 50)

	h.apply(t, quoteRequest(2, "0.52", "0.51", "0.49"))
	if got := h.working(); !slices.Equal(got, []int{49, 51, 52}) {
		t.Fatalf("working=%v", got)
	}
	if h.clob.postCount(0.52) != 1 || h.clob.postCount(0.51) != 1 {
		t.Fatalf("a kept level was posted again: 0.52=%d 0.51=%d", h.clob.postCount(0.52), h.clob.postCount(0.51))
	}
	if h.clob.postCount(0.49) != 1 {
		t.Fatalf("new level posts=%d", h.clob.postCount(0.49))
	}
	if got := h.cancels(); !slices.Equal(got, []string{dropped.ExchangeOrderID}) {
		t.Fatalf("cancels=%v, want only the dropped level %s", got, dropped.ExchangeOrderID)
	}
	if h.orderAt(t, 52).Revision != kept52.Revision || h.orderAt(t, 51).Revision != kept51.Revision {
		t.Fatal("a kept level's order changed")
	}
	if h.lane().done.Load() {
		t.Fatal("a requote ended the lane")
	}
}

func TestQuoteDropsStaleSeq(t *testing.T) {
	h := newQuoteHarness(t)
	h.apply(t, quoteRequest(5, "0.52"))
	h.apply(t, quoteRequest(4, "0.40"))
	h.apply(t, quoteRequest(5, "0.41"))
	if h.clob.postCount(0.40) != 0 || h.clob.postCount(0.41) != 0 {
		t.Fatal("a request with a seq not above the last one was applied")
	}
	if got := h.working(); !slices.Equal(got, []int{52}) {
		t.Fatalf("working=%v", got)
	}
}

func TestQuoteEmptyCancelsEverythingAndLaneQuotesAgain(t *testing.T) {
	h := newQuoteHarness(t)
	h.apply(t, quoteRequest(1, "0.52", "0.51"))
	h.apply(t, quoteRequest(2))
	if got := h.working(); len(got) != 0 {
		t.Fatalf("working=%v after an empty quote", got)
	}
	if len(h.cancels()) != 2 {
		t.Fatalf("cancels=%v", h.cancels())
	}
	h.clob.mu.Lock()
	batches := slices.Clone(h.clob.batchCancels)
	h.clob.mu.Unlock()
	if !slices.Equal(batches, []int{2}) {
		t.Fatalf("batch cancels=%v, want one call of 2", batches)
	}
	if h.lane().done.Load() {
		t.Fatal("an empty quote ended the lane")
	}
	h.apply(t, quoteRequest(3, "0.52"))
	if h.clob.postCount(0.52) != 2 {
		t.Fatalf("0.52 posts=%d, want a fresh post after the empty quote", h.clob.postCount(0.52))
	}
}

func TestQuoteDeadlineCancelsEverythingWithoutEndingLane(t *testing.T) {
	h := newQuoteHarness(t)
	req := quoteRequest(1, "0.52", "0.51")
	req.ExpiresAt = time.Now().Add(100 * time.Millisecond)
	h.apply(t, req)
	h.exec.checkQuotes(context.Background())
	if len(h.cancels()) != 0 {
		t.Fatal("cancelled before the deadline")
	}
	time.Sleep(120 * time.Millisecond)
	h.exec.checkQuotes(context.Background())
	eventually(t, "deadline cancels", func() bool { return len(h.cancels()) == 2 })
	h.idle(t)
	if got := h.working(); len(got) != 0 {
		t.Fatalf("working=%v after the deadline", got)
	}
	if h.lane().done.Load() {
		t.Fatal("the deadline ended the lane")
	}
	h.apply(t, quoteRequest(2, "0.51"))
	if h.clob.postCount(0.51) != 2 {
		t.Fatal("the lane did not quote again after its deadline")
	}
}

func TestQuoteFirstFillEndsLaneOnceWithOneBatchCancel(t *testing.T) {
	h := newQuoteHarness(t)
	h.apply(t, quoteRequest(1, "0.52", "0.51", "0.50"))
	filled := h.orderAt(t, 51)
	// The level fills in part on the exchange.
	h.store.mu.Lock()
	partial, err := h.store.move(filled, statemachine.EventPartialFillObserved, "4", "")
	h.store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := h.store.Intent(context.Background(), partial.IntentID)
	if !h.exec.quoteLanes.hasIntent(intent.IntentID) {
		t.Fatal("the lane's intent is not registered")
	}
	h.exec.QuoteOrderObserved(context.Background(), intent, partial, true)
	if !h.lane().done.Load() {
		t.Fatal("done is not set as soon as the fill is seen")
	}
	// A second observation and a later request change nothing.
	h.exec.QuoteOrderObserved(context.Background(), intent, partial, true)
	if err := h.exec.ApplyQuote(context.Background(), quoteRequest(2, "0.53")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "one result", func() bool { return len(h.quotes.published()) == 1 })
	eventually(t, "siblings cancelled", func() bool { return len(h.cancels()) == 3 })
	h.idle(t)
	time.Sleep(50 * time.Millisecond)
	results := h.quotes.published()
	if len(results) != 1 {
		t.Fatalf("results=%d, want exactly one", len(results))
	}
	result := results[0]
	if result.Price != "0.51" || result.RequestID != quoteRequest(1).RequestID || result.FilledShares != 4 || result.UniqueTag != "lane" {
		t.Fatalf("result=%+v", result)
	}
	h.clob.mu.Lock()
	batches := slices.Clone(h.clob.batchCancels)
	h.clob.mu.Unlock()
	if !slices.Equal(batches, []int{3}) {
		t.Fatalf("batch cancels=%v, want one call carrying all three levels", batches)
	}
	if h.clob.postCount(0.53) != 0 {
		t.Fatal("a done lane posted a new level")
	}
	if h.store.markDoneCalls != 1 {
		t.Fatalf("lane marked done %d times", h.store.markDoneCalls)
	}
}

func TestQuoteCrossedLevelParksAndRests(t *testing.T) {
	h := newQuoteHarness(t)
	h.clob.mu.Lock()
	h.clob.cross[0.52] = 2
	h.clob.mu.Unlock()
	h.apply(t, quoteRequest(1, "0.52", "0.51"))
	eventually(t, "the crossed level rests", func() bool {
		lane := h.lane()
		lane.mu.Lock()
		defer lane.mu.Unlock()
		level := lane.levels[52]
		return level != nil && !level.parked
	})
	if h.clob.postCount(0.52) != 3 {
		t.Fatalf("0.52 posts=%d, want 3 (two crosses, then rest)", h.clob.postCount(0.52))
	}
	// Wanted again while it rests: no further post.
	h.apply(t, quoteRequest(2, "0.52", "0.51"))
	if h.clob.postCount(0.52) != 3 || h.clob.postCount(0.51) != 1 {
		t.Fatal("a requote reposted a resting level")
	}
}

func TestQuoteParkedLevelThatLeavesWantIsCancelledLocally(t *testing.T) {
	h := newQuoteHarness(t)
	h.clob.forever(0.52)
	h.apply(t, quoteRequest(1, "0.52"))
	h.apply(t, quoteRequest(2, "0.51"))
	if got := h.working(); !slices.Equal(got, []int{51}) {
		t.Fatalf("working=%v", got)
	}
	posts := h.clob.postCount(0.52)
	time.Sleep(100 * time.Millisecond)
	if h.clob.postCount(0.52) != posts {
		t.Fatal("a level no longer wanted kept retrying")
	}
	if len(h.cancels()) != 0 {
		t.Fatal("a parked level was cancelled on the exchange")
	}
}

func TestQuoteParkedLevelCancelledOutsideTheLaneStopsWorking(t *testing.T) {
	// The lane scan of a close can cancel a parked level locally, behind the
	// lane: no account-feed event follows, so the next repost's conflict is
	// the lane's only sign that the level ended.
	h := newQuoteHarness(t)
	h.clob.forever(0.52)
	h.apply(t, quoteRequest(1, "0.52"))
	lane := h.lane()
	lane.mu.Lock()
	intent := lane.intent
	lane.mu.Unlock()
	eventually(t, "the parked level is cancelled", func() bool {
		order := h.orderAt(t, 52)
		if order.State == statemachine.StateRetryPending {
			_ = h.exec.cancelParkedOrder(context.Background(), intent, order, "lane scan")
		}
		order, err := h.store.OrderByIntent(context.Background(), order.IntentID, order.ChildSequence)
		return err == nil && order.State == statemachine.StateCanceled
	})
	eventually(t, "the cancelled level stops working", func() bool { return len(h.working()) == 0 })
}

func TestQuoteForceCloseWithdrawsLevels(t *testing.T) {
	h := newQuoteHarness(t)
	h.apply(t, quoteRequest(1, "0.52", "0.51"))
	h.exec.WithdrawQuote(context.Background(), "lane", "cond", "token")
	if got := h.working(); len(got) != 0 {
		t.Fatalf("working=%v after a force close", got)
	}
	if len(h.cancels()) != 2 {
		t.Fatalf("cancels=%v", h.cancels())
	}
	if h.lane().done.Load() {
		t.Fatal("a force close ended the lane")
	}
}

func TestQuoteRestartAdoptsLevelsKeepsDeadlineAndPublishesOwedResultOnce(t *testing.T) {
	h := newQuoteHarness(t)
	req := quoteRequest(1, "0.52", "0.51")
	req.ExpiresAt = time.Now().Add(150 * time.Millisecond)
	h.apply(t, req)
	_ = h.exec.Close(context.Background())

	// A new process on the same store and exchange.
	restarted, err := New(h.store, h.clob, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	sink := &quoteSink{}
	restarted.SetEventPublisher(sink)
	restarted.SetErrorHandler(func(err error) { t.Logf("executor error: %v", err) })
	t.Cleanup(func() { _ = restarted.Close(context.Background()) })
	if err := restarted.recoverOpenOrders(context.Background()); err != nil {
		t.Fatal(err)
	}
	lane := restarted.quoteLanes.lane(quoteKey)
	lane.mu.Lock()
	adopted := len(lane.levels)
	lastSeq := lane.lastSeq
	lane.mu.Unlock()
	if adopted != 2 || lastSeq != 1 {
		t.Fatalf("adopted %d levels at seq %d, want 2 at 1", adopted, lastSeq)
	}
	restarted.watchMu.Lock()
	watched := len(restarted.watched)
	restarted.watchMu.Unlock()
	if watched != 0 {
		t.Fatal("the generic watcher adopted quote levels")
	}
	time.Sleep(170 * time.Millisecond)
	restarted.checkQuotes(context.Background())
	eventually(t, "adopted levels cancelled at the deadline", func() bool { return len(h.cancels()) == 2 })

	// A result owed before the restart is published once.
	if _, err := h.store.MarkQuoteDone(context.Background(), quoteKey, lane.intent.IntentID, 1); err != nil {
		t.Fatal(err)
	}
	_ = h.store.RecordQuoteLevel(context.Background(), lane.intent.IntentID, 1, "req-owed")
	for range 3 {
		if err := restarted.recoverOpenOrders(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	results := sink.published()
	if len(results) != 1 || results[0].RequestID != "req-owed" {
		t.Fatalf("results=%+v, want the owed one once", results)
	}
}

func TestQuoteDoneLaneRefusesRequestsAfterRestart(t *testing.T) {
	h := newQuoteHarness(t)
	if _, err := h.store.MarkQuoteDone(context.Background(), quoteKey, "", 1); err != nil {
		t.Fatal(err)
	}
	_, _ = h.store.MoveQuoteResult(context.Background(), quoteKey, store.QuoteResultPublished, store.QuoteResultOwed)
	h.apply(t, quoteRequest(9, "0.52"))
	if h.clob.totalPosts() != 0 {
		t.Fatal("a lane done before the restart posted")
	}
}

func TestQuoteLevelsNeverPublishOpenResults(t *testing.T) {
	h := newQuoteHarness(t)
	sink := &resultSink{}
	h.exec.SetEventPublisher(sink)
	h.apply(t, quoteRequest(1, "0.52"))
	h.apply(t, quoteRequest(2))
	intent := h.lane().intent
	record, _ := h.store.Intent(context.Background(), intent.IntentID)
	if mapping.ExecutionIntent(record).Policy.Style != protocol.ExecutionStyleQuote {
		t.Fatalf("stored intent style %q", mapping.ExecutionIntent(record).Policy.Style)
	}
	if len(sink.published()) != 0 {
		t.Fatalf("a quote published open results: %+v", sink.published())
	}
}

func TestQuoteIdleLanesArePrunedAndReloadFromTheStore(t *testing.T) {
	h := newQuoteHarness(t)
	req := quoteRequest(3, "0.52")
	req.ExpiresAt = time.Now().Add(time.Second)
	h.apply(t, req)
	h.apply(t, quoteRequest(4))
	h.exec.pruneQuoteLanes(time.Now().Add(quoteLaneIdle))
	if len(h.exec.quoteLanes.snapshot()) != 1 {
		t.Fatal("a lane inside its idle window was pruned")
	}
	h.exec.pruneQuoteLanes(time.Now().Add(quoteLaneIdle + 5*time.Second))
	if len(h.exec.quoteLanes.snapshot()) != 0 {
		t.Fatal("an idle lane was kept")
	}
	// Reloaded from the store: a stale seq is still refused.
	h.apply(t, quoteRequest(2, "0.40"))
	if h.clob.postCount(0.40) != 0 {
		t.Fatal("a pruned lane forgot its seq")
	}
	h.apply(t, quoteRequest(5, "0.41"))
	if h.clob.postCount(0.41) != 1 {
		t.Fatal("a pruned lane did not quote again")
	}
}

// restartQuotes starts a second executor on the harness's store and exchange,
// as a process restarted with the lane's orders still open.
func (h *quoteHarness) restart(t *testing.T) (*Executor, *quoteSink) {
	t.Helper()
	_ = h.exec.Close(context.Background())
	restarted, err := New(h.store, h.clob, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	sink := &quoteSink{}
	restarted.SetEventPublisher(sink)
	restarted.SetErrorHandler(func(err error) { t.Logf("executor error: %v", err) })
	t.Cleanup(func() { _ = restarted.Close(context.Background()) })
	return restarted, sink
}

func cancelOpen() protocol.ExecutionCloseRequest {
	return protocol.ExecutionCloseRequest{
		SchemaVersion: protocol.SchemaVersionV1, UniqueTag: "lane", Strategy: "strategy", ConditionID: "cond",
		AssetID: "token", Outcome: "Up", Mode: protocol.ExecutionCloseModeCancelOpen,
	}
}

func TestQuoteCancelOpenReachesEveryLevelEvenWhenTheLaneIsNotLoaded(t *testing.T) {
	h := newQuoteHarness(t)
	h.clob.forever(0.50)
	h.apply(t, quoteRequest(1, "0.52", "0.51", "0.50"))
	h.apply(t, quoteRequest(2, "0.52", "0.51", "0.50", "0.49"))
	restarted, _ := h.restart(t)
	if err := restarted.ExecuteClose(context.Background(), cancelOpen()); err != nil {
		t.Fatal(err)
	}
	intentID := h.lane().intent.IntentID
	states := h.store.states(intentID)
	for sequence, state := range states {
		if state == statemachine.StateLive || state == statemachine.StateRetryPending {
			t.Fatalf("level %d still %s after CANCEL_OPEN: %v", sequence, state, states)
		}
	}
	if len(h.cancels()) != 3 {
		t.Fatalf("cancels=%v, want the three resting levels", h.cancels())
	}
}

func TestQuoteCancelOpenOnALoadedLaneStopsItsRetries(t *testing.T) {
	h := newQuoteHarness(t)
	h.clob.forever(0.52)
	h.apply(t, quoteRequest(1, "0.52", "0.51"))
	if err := h.exec.ExecuteClose(context.Background(), cancelOpen()); err != nil {
		t.Fatal(err)
	}
	if got := h.working(); len(got) != 0 {
		t.Fatalf("working=%v", got)
	}
	posts := h.clob.postCount(0.52)
	time.Sleep(100 * time.Millisecond)
	if h.clob.postCount(0.52) != posts {
		t.Fatal("a parked level kept retrying after CANCEL_OPEN")
	}
}

func TestQuoteLevelWithAFailedCancelIsNotPostedTwice(t *testing.T) {
	h := newQuoteHarness(t)
	h.apply(t, quoteRequest(1, "0.52"))
	lane := h.lane()
	lane.mu.Lock()
	lane.levels[52].cancelling = true
	lane.mu.Unlock()
	h.apply(t, quoteRequest(2, "0.52", "0.51"))
	if h.clob.postCount(0.52) != 1 {
		t.Fatal("a price whose old order may still rest was posted again")
	}
	if len(h.cancels()) != 0 {
		t.Fatal("the request cancelled the stuck level itself; that is the deadline pass's to retry")
	}
	h.exec.checkQuotes(context.Background())
	eventually(t, "stuck level cancelled", func() bool { return len(h.cancels()) == 1 })
	h.idle(t)
	h.apply(t, quoteRequest(3, "0.52", "0.51"))
	if h.clob.postCount(0.52) != 2 {
		t.Fatal("the price was not quoted again once its old order was gone")
	}
}

func TestQuoteRestoreRetiresASecondOrderAtOnePrice(t *testing.T) {
	h := newQuoteHarness(t)
	h.apply(t, quoteRequest(1, "0.52"))
	intentID := h.lane().intent.IntentID
	// Another open order at 0.52 under the same lane, as a failed cancel
	// would have left behind.
	older := h.orderAt(t, 52)
	older.ChildSequence, older.ExchangeOrderID = 99, "order-older"
	h.store.mu.Lock()
	h.store.orders[orderKey{intentID, 99}] = older
	h.store.mu.Unlock()
	restarted, _ := h.restart(t)
	if err := restarted.recoverOpenOrders(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted.checkQuotes(context.Background())
	eventually(t, "one of the two orders cancelled", func() bool { return len(h.cancels()) == 1 })
}

func TestQuoteFirstPostWaitsForTheDurableLaneLink(t *testing.T) {
	h := newQuoteHarness(t)
	h.store.quoteMu.Lock()
	h.store.failSave = errors.New("database down")
	h.store.quoteMu.Unlock()
	h.apply(t, quoteRequest(1, "0.52"))
	if h.clob.totalPosts() != 0 {
		t.Fatal("levels were posted although the lane's link to its intent was not stored")
	}
}

func TestQuoteResultPublishFailureLeavesItOwed(t *testing.T) {
	h := newQuoteHarness(t)
	h.apply(t, quoteRequest(1, "0.52"))
	order := h.orderAt(t, 52)
	h.store.mu.Lock()
	filled, err := h.store.move(order, statemachine.EventFillObserved, order.RequestedShares, "")
	h.store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.quotes.mu.Lock()
	h.quotes.fail = errors.New("nats down")
	h.quotes.mu.Unlock()
	intent, _ := h.store.Intent(context.Background(), filled.IntentID)
	h.exec.QuoteOrderObserved(context.Background(), intent, filled, true)
	eventually(t, "lane marked done", func() bool {
		lane, _, _ := h.store.QuoteLane(context.Background(), quoteKey)
		return lane.Done
	})
	time.Sleep(50 * time.Millisecond)
	if lane, _, _ := h.store.QuoteLane(context.Background(), quoteKey); lane.ResultState != store.QuoteResultOwed {
		t.Fatalf("result state %q after a failed publish, want OWED", lane.ResultState)
	}
	h.quotes.mu.Lock()
	h.quotes.fail = nil
	h.quotes.mu.Unlock()
	for range 2 {
		if err := h.exec.recoverOpenOrders(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	results := h.quotes.published()
	if len(results) != 1 || results[0].AveragePrice != 0.52 {
		t.Fatalf("results=%+v, want one, priced at the level before its trade landed", results)
	}
}

func driftQuotes(mid float64) fakeQuotes {
	return fakeQuotes{has: true, snapshot: marketquotes.Snapshot{ConditionID: "cond", At: time.Now(),
		Up: marketquotes.Quote{AssetID: "token", Bid: mid - 0.005, Ask: mid + 0.005, Mid: mid, Timestamp: time.Now()}}}
}

func TestQuoteDriftSweepDecidesAgainstTheNewestSignal(t *testing.T) {
	h := newQuoteHarness(t)
	h.exec.SetQuoteProvider(driftQuotes(0.50))
	req := quoteRequest(1, "0.48")
	req.SignalPrice, req.MaxSignalDrift = "0.50", "0.03"
	h.apply(t, req)
	if got := h.working(); len(got) != 1 {
		t.Fatalf("working=%v", got)
	}
	// The market falls past the bound of the first signal, and the strategy
	// re-states the quote from the new mid.
	h.exec.SetQuoteProvider(driftQuotes(0.45))
	if reason := h.exec.quoteSweepReason(h.lane()); reason != "drift" {
		t.Fatalf("reason=%q against the old signal", reason)
	}
	req = quoteRequest(2, "0.48")
	req.SignalPrice, req.MaxSignalDrift = "0.45", "0.03"
	h.apply(t, req)
	if reason := h.exec.quoteSweepReason(h.lane()); reason != "" {
		t.Fatalf("reason=%q against the newest signal", reason)
	}
	h.exec.checkQuotes(context.Background())
	time.Sleep(50 * time.Millisecond)
	if len(h.cancels()) != 0 {
		t.Fatal("the drift sweep cancelled a level the newest request kept")
	}
}

func TestQuoteRestoreDropsTheStoredDriftBound(t *testing.T) {
	h := newQuoteHarness(t)
	h.exec.SetQuoteProvider(driftQuotes(0.50))
	req := quoteRequest(1, "0.48")
	req.SignalPrice, req.MaxSignalDrift = "0.50", "0.03"
	h.apply(t, req)
	restarted, _ := h.restart(t)
	restarted.SetQuoteProvider(driftQuotes(0.30))
	if err := restarted.recoverOpenOrders(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted.checkQuotes(context.Background())
	time.Sleep(50 * time.Millisecond)
	if len(h.cancels()) != 0 {
		t.Fatal("a restored level was cancelled for the drift bound of an old request")
	}
}
