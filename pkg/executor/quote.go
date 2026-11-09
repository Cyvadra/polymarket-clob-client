package executor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/mapping"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/accountfeed"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor/tactics"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// A quote lane (unique_tag, condition_id, token_id) is a strategy making a
// market: every ExecutionQuoteRequest says which whole cents it wants a
// post-only GTC buy resting at, and the lane keeps the orders to match.
//
//   - A level wanted again is left alone, with no exchange call, so it keeps
//     its place in the exchange's FIFO queue. A level no longer wanted is
//     cancelled, and a newly wanted one is posted. A level that would cross
//     parks and is reposted, as a ladder level is.
//   - Requests apply in seq order. A stale one is dropped, and the newest
//     waiting request replaces any older one waiting behind the lane's
//     current operation.
//   - Passing expires_at, or a signal drift, cancels every level. Neither
//     ends the lane: a later request quotes again.
//   - The first fill on any level ends the lane for good. done is set before
//     any exchange call, every other level and the rest of the filled one are
//     cancelled in one batch, and one ExecutionQuoteResult is published.
//
// The lane's levels are the child orders of its one QUOTE intent, which is
// stored the first time it posts. Each level takes the next child sequence, so
// a price quoted, cancelled and quoted again is a new child. The whole quote is
// therefore one lot in open_lots.
//
// Two locks guard a lane. ops serialises every operation that posts, cancels
// or reposts, so an apply always sees what the previous one left. mu guards
// the fields and is only held briefly, so the deadline pass and the first-fill
// path read a lane without waiting out an exchange call.

// quoteLevel is one working level of a lane.
type quoteLevel struct {
	cents     int
	sequence  int
	requestID string
	// parked is set while the level waits in RETRY_PENDING, and order is
	// then the row to repost from.
	parked bool
	order  store.SignedOrderRecord
	// cancelling marks a level whose cancel failed. It no longer counts as
	// wanted, and the next deadline pass tries the cancel again.
	cancelling bool
}

type quoteLane struct {
	key  store.QuoteLaneKey
	done atomic.Bool
	ops  sync.Mutex
	// sweeping is set while a deadline, drift or end-of-lane cancel runs, so
	// the deadline pass starts one at a time.
	sweeping atomic.Bool

	mu       sync.Mutex
	loaded   bool
	pending  *pendingQuote
	draining bool
	retrying bool
	lastSeq  int64
	deadline time.Time
	// intent is the lane's QUOTE intent as the latest request sizes it. Its
	// IntentID is empty until the lane first posts.
	intent protocol.ExecutionIntent
	levels map[int]*quoteLevel // by cents
	// stuck holds levels whose cancel failed while another level took their
	// cents. They are no longer wanted, and every sweep retries their cancel.
	stuck   []*quoteLevel
	nextSeq int
	// pruned is set, under mu and the registry lock, when the registry drops
	// the lane. A caller holding the old pointer must fetch the lane again.
	pruned bool
	// filledAt is when the first fill was seen, for the fill-to-cancel metric.
	filledAt time.Time
}

// pendingQuote is a validated request waiting for its lane.
type pendingQuote struct {
	req  protocol.ExecutionQuoteRequest
	want []int
	// receivedAt times the request's pre-post latency from its arrival.
	receivedAt time.Time
}

// quoteRegistry holds the lanes this process knows.
type quoteRegistry struct {
	mu   sync.Mutex
	stop chan struct{}
	// publishing holds the lanes whose result is being published here, so
	// the live path and the recovery pass never publish one at once.
	publishing map[store.QuoteLaneKey]bool
	lanes      map[store.QuoteLaneKey]*quoteLane
	byIntent   map[string]*quoteLane
	closed     bool
	wg         sync.WaitGroup
}

func (r *quoteRegistry) lane(key store.QuoteLaneKey) *quoteLane {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lanes == nil {
		r.lanes, r.byIntent = map[store.QuoteLaneKey]*quoteLane{}, map[string]*quoteLane{}
	}
	lane, ok := r.lanes[key]
	if !ok {
		lane = &quoteLane{key: key, levels: map[int]*quoteLevel{}, nextSeq: 1}
		r.lanes[key] = lane
	}
	return lane
}

func (r *quoteRegistry) bindIntent(lane *quoteLane, intentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byIntent == nil {
		r.byIntent = map[string]*quoteLane{}
	}
	r.byIntent[intentID] = lane
}

func (r *quoteRegistry) unbindIntent(lane *quoteLane, intentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byIntent[intentID] == lane {
		delete(r.byIntent, intentID)
	}
}

func (r *quoteRegistry) hasIntent(intentID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.byIntent[intentID]
	return ok
}

func (r *quoteRegistry) snapshot() []*quoteLane {
	r.mu.Lock()
	defer r.mu.Unlock()
	lanes := make([]*quoteLane, 0, len(r.lanes))
	for _, lane := range r.lanes {
		lanes = append(lanes, lane)
	}
	return lanes
}

// goRun starts background work for a lane unless the executor is closing.
func (r *quoteRegistry) goRun(fn func()) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		fn()
	}()
	return true
}

// stopped is closed once the executor is closing.
func (r *quoteRegistry) stopped() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop == nil {
		r.stop = make(chan struct{})
	}
	return r.stop
}

func (r *quoteRegistry) closeAll() {
	r.mu.Lock()
	if r.stop == nil {
		r.stop = make(chan struct{})
	}
	if !r.closed {
		close(r.stop)
	}
	r.closed = true
	r.mu.Unlock()
	r.wg.Wait()
}

func quoteKeyOf(tag, conditionID, tokenID string) store.QuoteLaneKey {
	return store.QuoteLaneKey{UniqueTag: tag, ConditionID: conditionID, TokenID: tokenID}
}

// QuoteMetrics counts what quote lanes do. Each field may be nil.
type QuoteMetrics struct {
	// Levels counts level posts and cancels: action is "post" or "cancel",
	// reason one of requote, empty, deadline, drift, fill, force_close.
	Levels func(action, reason string, n int)
	// FillToCancel is the time from a lane's first fill being seen to the
	// exchange answering the batch cancel of its other levels.
	FillToCancel func(time.Duration)
}

// SetQuoteMetrics installs the quote lane counters. Set it before Run.
func (e *Executor) SetQuoteMetrics(metrics QuoteMetrics) { e.quoteMetrics = metrics }

func (e *Executor) countLevels(action, reason string, n int) {
	if n > 0 && e.quoteMetrics.Levels != nil {
		e.quoteMetrics.Levels(action, reason, n)
	}
}

// validateQuote checks a request and returns its levels in cents, highest
// first. An invalid request changes nothing.
func validateQuote(req protocol.ExecutionQuoteRequest, maxLevels int) ([]int, error) {
	if req.SchemaVersion != protocol.SchemaVersionV1 {
		return nil, invalid("unsupported quote schema version %q", req.SchemaVersion)
	}
	if strings.TrimSpace(req.UniqueTag) == "" || req.Strategy == "" || req.ConditionID == "" || req.TokenID == "" || req.Outcome == "" {
		return nil, invalid("quote unique tag, strategy, condition ID, token ID, and outcome are required")
	}
	if req.Side != protocol.SideBuy {
		return nil, invalid("a quote must buy, not %q", req.Side)
	}
	if !req.PostOnly {
		return nil, invalid("a quote must be post-only")
	}
	if req.Seq <= 0 {
		return nil, invalid("quote seq must be positive")
	}
	if req.ExpiresAt.IsZero() {
		return nil, invalid("quote expires_at is required")
	}
	if len(req.Levels) > maxLevels {
		return nil, invalid("quote has %d levels, over the limit of %d", len(req.Levels), maxLevels)
	}
	levels := make([]int, 0, len(req.Levels))
	for i, text := range req.Levels {
		cents, ok := wholeCents(text)
		if !ok || cents < 1 || cents >= centsPerUnit {
			return nil, invalid("quote level %q is not a whole cent in (0, 1)", text)
		}
		if i > 0 && cents >= levels[i-1] {
			return nil, invalid("quote levels must be distinct and highest first, got %v", req.Levels)
		}
		levels = append(levels, cents)
	}
	if len(levels) > 0 {
		switch {
		case req.TargetUSD != "" && req.TargetEquityFraction != "":
			return nil, invalid("target_usd and target_equity_fraction are mutually exclusive")
		case req.TargetUSD == "" && req.TargetEquityFraction == "":
			return nil, invalid("a non-empty quote needs target_usd or target_equity_fraction")
		case req.TargetUSD != "":
			if _, err := decimal.PositiveFloat(req.TargetUSD); err != nil {
				return nil, invalid("invalid target usd %q", req.TargetUSD)
			}
		}
	}
	if _, err := tactics.ParseDrift(protocol.ExecutionIntent{SignalPrice: req.SignalPrice, MaxSignalDrift: req.MaxSignalDrift, Side: req.Side, TimeInForce: protocol.TimeInForceGTC, Kind: protocol.IntentOpen}); err != nil {
		return nil, invalid("%s", err)
	}
	return levels, nil
}

// ApplyQuote takes a quote request. It returns at once: the lane applies its
// requests in order on its own goroutine, so one lane's exchange calls never
// hold up another's. The error is only for the log; a quote publishes nothing
// but its first fill.
func (e *Executor) ApplyQuote(ctx context.Context, req protocol.ExecutionQuoteRequest) error {
	want, err := validateQuote(req, e.ladderLevelLimit())
	if err != nil {
		return fmt.Errorf("drop quote %s seq %d for %s: %w", req.RequestID, req.Seq, req.UniqueTag, err)
	}
	received := time.Now()
	var lane *quoteLane
	for {
		lane = e.quoteLanes.lane(quoteKeyOf(req.UniqueTag, req.ConditionID, req.TokenID))
		if lane.done.Load() {
			return nil
		}
		lane.mu.Lock()
		if !lane.pruned {
			break
		}
		// Pruned between the lookup and the lock: one key, one lane.
		lane.mu.Unlock()
	}
	if lane.loaded && req.Seq <= lane.lastSeq {
		lane.mu.Unlock()
		return nil
	}
	if lane.pending != nil && req.Seq <= lane.pending.req.Seq {
		lane.mu.Unlock()
		return nil
	}
	lane.pending = &pendingQuote{req: req, want: want, receivedAt: received}
	start := !lane.draining
	lane.draining = true
	lane.mu.Unlock()
	if start {
		base := context.WithoutCancel(ctx)
		if !e.quoteLanes.goRun(func() { e.drainQuote(base, lane) }) {
			lane.mu.Lock()
			lane.draining = false
			lane.mu.Unlock()
			return errExecutorClosed
		}
	}
	return nil
}

// drainQuote applies a lane's waiting requests until none is left. Only the
// newest waiting request is kept, since only the newest want matters.
func (e *Executor) drainQuote(ctx context.Context, lane *quoteLane) {
	for {
		lane.ops.Lock()
		if err := e.loadQuoteLane(ctx, lane); err != nil {
			lane.ops.Unlock()
			e.reportError(err)
			lane.mu.Lock()
			lane.pending, lane.draining = nil, false
			lane.mu.Unlock()
			return
		}
		lane.mu.Lock()
		next := lane.pending
		lane.pending = nil
		if next == nil {
			lane.draining = false
			lane.mu.Unlock()
			lane.ops.Unlock()
			return
		}
		lane.mu.Unlock()
		applyCtx := context.WithValue(ctx, receivedAtKey{}, next.receivedAt)
		if err := e.applyQuote(applyCtx, lane, next.req, next.want); err != nil {
			e.reportError(fmt.Errorf("quote %s seq %d for %s: %w", next.req.RequestID, next.req.Seq, next.req.UniqueTag, err))
		}
		lane.ops.Unlock()
	}
}

// loadQuoteLane reads a lane this process has not seen since it started: its
// last seq, deadline and done flag, and its working levels. A level that
// filled while the process was down ends the lane here. Called with ops held.
func (e *Executor) loadQuoteLane(ctx context.Context, lane *quoteLane) error {
	lane.mu.Lock()
	loaded := lane.loaded
	lane.mu.Unlock()
	if loaded {
		return nil
	}
	record, found, err := e.store.QuoteLane(ctx, lane.key)
	if err != nil {
		return fmt.Errorf("load quote lane %v: %w", lane.key, err)
	}
	return e.restoreQuoteLane(ctx, lane, record, found)
}

// restoreQuoteLane rebuilds a lane from its stored record and its intent's
// child orders. Called with ops held.
func (e *Executor) restoreQuoteLane(ctx context.Context, lane *quoteLane, record store.QuoteLaneRecord, found bool) error {
	var (
		intent  protocol.ExecutionIntent
		orders  []store.SignedOrderRecord
		intentR store.OrderIntentRecord
	)
	if found && record.IntentID != "" {
		var err error
		if intentR, err = e.store.Intent(ctx, record.IntentID); err != nil {
			return fmt.Errorf("load quote intent %s: %w", record.IntentID, err)
		}
		intent = mapping.ExecutionIntent(intentR)
		if orders, err = e.store.IntentOrders(ctx, record.IntentID); err != nil {
			return fmt.Errorf("load quote levels of %s: %w", record.IntentID, err)
		}
		e.quoteLanes.bindIntent(lane, record.IntentID)
	}
	var filled *store.SignedOrderRecord
	lane.mu.Lock()
	lane.loaded = true
	if found {
		lane.lastSeq = max(lane.lastSeq, record.LastSeq)
		lane.deadline = record.Deadline
		// The stored intent carries the drift bound of the lane's first
		// request, which may be hours old. Until the strategy re-states the
		// quote, only the deadline applies.
		intent.SignalPrice, intent.MaxSignalDrift = "", ""
		lane.intent = intent
		if record.Done {
			lane.done.Store(true)
		}
	}
	for _, order := range orders {
		lane.nextSeq = max(lane.nextSeq, order.ChildSequence+1)
		if decimal.Positive(order.MatchedShares) && filled == nil {
			filled = &order
		}
		if statemachine.IsTerminal(order.State) {
			continue
		}
		cents, ok := wholeCents(order.Price)
		if !ok {
			continue
		}
		level := &quoteLevel{cents: cents, sequence: order.ChildSequence, parked: order.State == statemachine.StateRetryPending, order: order}
		if older, taken := lane.levels[cents]; taken {
			// Two open orders at one price: keep the newer, retire the older.
			older.cancelling, older.parked = true, false
			lane.stuck = append(lane.stuck, older)
		}
		lane.levels[cents] = level
	}
	parked := lane.hasParkedLocked()
	lane.mu.Unlock()
	switch {
	case lane.done.Load():
		// Done before the restart, or by a fill seen just now: the cancels
		// that followed may not all have landed.
		e.endQuoteLevels(ctx, lane, "fill")
	case filled != nil:
		// The fill arrived while the process was down.
		e.quoteOrderSeen(ctx, intent, *filled, true)
	case parked:
		e.startQuoteRetry(lane)
	}
	return nil
}

// quoteSuspended reports whether the strategy is over a drawdown limit.
func (e *Executor) quoteSuspended(strategy string) bool {
	if e.gate == nil {
		return false
	}
	suspended, _ := e.gate.Suspended(strategy)
	return suspended
}

func (l *quoteLane) hasParkedLocked() bool {
	for _, level := range l.levels {
		if level.parked {
			return true
		}
	}
	return false
}

// applyQuote diffs one request against the lane's working levels. Called
// with ops held.
func (e *Executor) applyQuote(ctx context.Context, lane *quoteLane, req protocol.ExecutionQuoteRequest, want []int) error {
	if lane.done.Load() {
		return nil
	}
	now := e.now().UTC()
	lane.mu.Lock()
	if req.Seq <= lane.lastSeq {
		lane.mu.Unlock()
		return nil
	}
	prevSeq, prevDeadline := lane.lastSeq, lane.deadline
	lane.lastSeq = req.Seq
	lane.deadline = req.ExpiresAt.UTC()
	intent := lane.intent
	lane.mu.Unlock()

	// The request as an intent: the stored one keeps its ID and provenance,
	// while size, drift bound and deadline follow the newest request.
	next := e.quoteIntent(req, intent.IntentID, want)
	reason := "requote"
	switch {
	case len(want) == 0:
		reason = "empty"
	case !now.Before(req.ExpiresAt):
		// Already past its deadline: nothing may rest.
		want, reason = nil, "deadline"
	case e.checkOpenDrift(next, now) != nil:
		want, reason = nil, "drift"
	case e.quoteSuspended(req.Strategy):
		// Over its drawdown limit: the same gate ExecuteOpen applies. Nothing
		// new may rest, and what rests comes down.
		want, reason = nil, "drawdown"
	}
	created := false
	if len(want) > 0 && intent.IntentID == "" {
		created = true
		next.IntentID = newExecutionID()
		if _, err := e.store.InsertIntent(ctx, mapping.IntentRecord(next, now)); err != nil {
			return fmt.Errorf("persist quote intent: %w", err)
		}
		e.quoteLanes.bindIntent(lane, next.IntentID)
	}
	lane.mu.Lock()
	if next.IntentID != "" {
		lane.intent = next
	}
	wanted := map[int]bool{}
	for _, cents := range want {
		wanted[cents] = true
	}
	var stale []*quoteLevel
	var adds []int
	for cents, level := range lane.levels {
		// A level whose cancel failed is the deadline pass's to retry. Its
		// cents are not posted again until it is gone, so one price never
		// has two orders.
		if !wanted[cents] && !level.cancelling {
			stale = append(stale, level)
		}
	}
	for _, cents := range want {
		if _, ok := lane.levels[cents]; !ok {
			adds = append(adds, cents)
		}
	}
	lane.mu.Unlock()
	// Only the write that links a new intent carries it, and that one
	// commits durably before anything is posted (SaveQuoteLane).
	saved := store.QuoteLaneRecord{Key: lane.key, LastSeq: req.Seq, Deadline: req.ExpiresAt.UTC()}
	if created {
		saved.IntentID = next.IntentID
	}
	if err := e.store.SaveQuoteLane(ctx, saved); err != nil {
		if created {
			// Unlink the new intent so the next request creates and links
			// one again, rather than posting under an intent quote_lanes
			// never learned (and recovery would never adopt).
			lane.mu.Lock()
			lane.intent = intent
			if lane.lastSeq == req.Seq {
				lane.lastSeq, lane.deadline = prevSeq, prevDeadline
			}
			lane.mu.Unlock()
			e.quoteLanes.unbindIntent(lane, next.IntentID)
			return fmt.Errorf("link quote lane to its intent: %w", err)
		}
		e.reportError(err)
	}

	// Cancels and posts go out together: one batch call each.
	var wg sync.WaitGroup
	var cancelErr error
	if len(stale) > 0 {
		wg.Go(func() { cancelErr = e.cancelQuoteLevels(ctx, lane, stale, reason) })
	}
	var postErr error
	if len(adds) > 0 {
		postErr = e.postQuoteLevels(ctx, lane, req, next, adds)
	}
	wg.Wait()
	if lane.done.Load() {
		// The first fill landed while this request was posting: whatever it
		// posted must come down too.
		e.endQuoteLevels(ctx, lane, "fill")
	}
	return errors.Join(cancelErr, postErr)
}

// quoteIntent is the intent a request's levels are posted under.
func (e *Executor) quoteIntent(req protocol.ExecutionQuoteRequest, intentID string, want []int) protocol.ExecutionIntent {
	limit := centsPrice(centsPerUnit - 1)
	if len(want) > 0 {
		limit = centsPrice(want[0])
	}
	return protocol.ExecutionIntent{
		SchemaVersion: protocol.SchemaVersionV1, IntentID: intentID, RequestID: req.RequestID,
		UniqueTag: req.UniqueTag, Strategy: req.Strategy, Kind: protocol.IntentOpen,
		MarketID: req.MarketID, EventSlug: req.EventSlug, ConditionID: req.ConditionID, TokenID: req.TokenID,
		Outcome: req.Outcome, Side: protocol.SideBuy,
		TargetUSD:   scaleSize(req.TargetUSD, e.sizeMultiplier(req.Strategy), usdDigits),
		LimitPrice:  limit,
		TimeInForce: protocol.TimeInForceGTC, PostOnly: true,
		FeatureSeq: req.FeatureSeq, FeatureCompletedAt: req.FeatureCompletedAt,
		CreatedAt: req.CreatedAt, ExpiresAt: req.ExpiresAt,
		SignalPrice: req.SignalPrice, MaxSignalDrift: req.MaxSignalDrift,
		Policy: protocol.ExecutionPolicy{Style: protocol.ExecutionStyleQuote},
	}
}

// postQuoteLevels plans, reserves and posts the newly wanted levels in one
// batch. A level refused for crossing parks and is reposted; one refused for
// any other reason is not added, so a later request tries it again.
func (e *Executor) postQuoteLevels(ctx context.Context, lane *quoteLane, req protocol.ExecutionQuoteRequest, intent protocol.ExecutionIntent, adds []int) error {
	if req.TargetEquityFraction != "" {
		open := protocol.ExecutionOpenRequest{Strategy: req.Strategy, Side: protocol.SideBuy, TargetEquityFraction: req.TargetEquityFraction}
		release, err := e.sizeEntry(ctx, open, &intent)
		if err != nil {
			return err
		}
		defer release()
	}
	market, err := e.marketRules(ctx, intent.TokenID)
	if err != nil {
		return err
	}
	if err := checkLadderGrid(market.TickSize); err != nil {
		return err
	}
	now := e.now().UTC()
	level := intent
	level.Policy.Style = protocol.ExecutionStyleLimit
	lane.mu.Lock()
	children := make([]plannedChild, 0, len(adds))
	cents := map[int]int{} // sequence -> cents
	var tooSmall error
	for _, c := range adds {
		level.LimitPrice = centsPrice(c)
		decision, err := tactics.Plan(tactics.Request{Intent: level, Market: market, Now: now})
		switch {
		case err == nil:
			sequence := lane.nextSeq
			lane.nextSeq++
			cents[sequence] = c
			children = append(children, plannedChild{
				Sequence: sequence, Shares: decision.Shares, Price: decision.Price,
				PostOnly: decision.PostOnly, TimeInForce: decision.TimeInForce,
				ReservationReason: "quote level accepted",
			})
		case tactics.BelowMinOrderSize(err):
			tooSmall = err
		default:
			lane.mu.Unlock()
			return rejection{code: protocol.ReasonUnplannable, reason: err.Error(), cause: err}
		}
	}
	lane.mu.Unlock()
	if len(children) == 0 {
		return tooSmall
	}
	parked, placed, err := e.postLevels(context.Background(), withoutCashHold(ctx), intent, children)
	lane.mu.Lock()
	for _, sequence := range placed {
		lane.levels[cents[sequence]] = &quoteLevel{cents: cents[sequence], sequence: sequence, requestID: req.RequestID}
	}
	for sequence, order := range parked {
		lane.levels[cents[sequence]] = &quoteLevel{cents: cents[sequence], sequence: sequence, requestID: req.RequestID, parked: true, order: order}
	}
	lane.mu.Unlock()
	e.countLevels("post", "requote", len(placed)+len(parked))
	for _, sequence := range append(slices.Collect(maps.Keys(parked)), placed...) {
		if recErr := e.store.RecordQuoteLevel(ctx, intent.IntentID, sequence, req.RequestID); recErr != nil {
			e.reportError(recErr)
		}
	}
	if len(parked) > 0 {
		e.startQuoteRetry(lane)
	}
	return err
}

// cancelQuoteLevels cancels levels together: resting ones in one batch call,
// parked ones locally, in-flight ones by their order ID. A level whose cancel
// failed stays, marked cancelling, for the next deadline pass. The levels are
// removed from the lane before any call, so a request seeing the lane meanwhile
// does not count them as resting.
func (e *Executor) cancelQuoteLevels(ctx context.Context, lane *quoteLane, levels []*quoteLevel, reason string) error {
	if len(levels) == 0 {
		return nil
	}
	lane.mu.Lock()
	intent := lane.intent
	for _, level := range levels {
		if lane.levels[level.cents] == level {
			delete(lane.levels, level.cents)
		}
		lane.stuck = slices.DeleteFunc(lane.stuck, func(l *quoteLevel) bool { return l == level })
	}
	lane.mu.Unlock()
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		errs   error
		failed []*quoteLevel
	)
	members := e.newCancelMembers(len(levels))
	for i, level := range levels {
		member := members[i]
		wg.Go(func() {
			defer member.leave()
			if err := e.cancelQuoteLevel(context.WithValue(ctx, cancelMemberKey{}, member), intent, level, reason); err != nil {
				mu.Lock()
				errs = errors.Join(errs, err)
				failed = append(failed, level)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(failed) > 0 {
		lane.mu.Lock()
		for _, level := range failed {
			// No longer parked: the retry loop must not pick it up again at
			// once. The deadline sweep retries cancelling levels on its pace.
			level.cancelling, level.parked = true, false
			if _, taken := lane.levels[level.cents]; !taken {
				lane.levels[level.cents] = level
			} else {
				lane.stuck = append(lane.stuck, level)
			}
		}
		lane.mu.Unlock()
	}
	e.countLevels("cancel", reason, len(levels)-len(failed))
	return errs
}

// cancelQuoteLevel cancels one level from its current row.
func (e *Executor) cancelQuoteLevel(ctx context.Context, intent protocol.ExecutionIntent, level *quoteLevel, reason string) error {
	order, err := e.store.OrderByIntent(ctx, intent.IntentID, level.sequence)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reload quote level %s/%d: %w", intent.IntentID, level.sequence, err)
	}
	why := cancelRequested + ": quote " + reason
	switch order.State {
	case statemachine.StateRetryPending:
		return e.cancelParkedOrder(ctx, intent, order, why)
	case statemachine.StateSubmitting, statemachine.StateSubmitUnknown:
		return e.cancelSubmittingOrder(ctx, intent.Policy, order)
	}
	return e.cancelOpenOrder(ctx, intent.Policy, order, why)
}

// endQuoteLevels cancels every level of a lane. Called with ops held, or
// with ops free when the caller only wants the levels gone soon.
func (e *Executor) endQuoteLevels(ctx context.Context, lane *quoteLane, reason string) {
	lane.mu.Lock()
	levels := slices.Clone(lane.stuck)
	for _, level := range lane.levels {
		levels = append(levels, level)
	}
	lane.mu.Unlock()
	if err := e.cancelQuoteLevels(ctx, lane, levels, reason); err != nil {
		e.reportError(fmt.Errorf("cancel quote levels of %v (%s): %w", lane.key, reason, err))
	}
}

// startQuoteRetry starts the lane's repost loop unless it is running.
func (e *Executor) startQuoteRetry(lane *quoteLane) {
	lane.mu.Lock()
	if lane.retrying {
		lane.mu.Unlock()
		return
	}
	lane.retrying = true
	lane.mu.Unlock()
	if !e.quoteLanes.goRun(func() { e.retryQuote(lane) }) {
		lane.mu.Lock()
		lane.retrying = false
		lane.mu.Unlock()
	}
}

// retryQuote reposts a lane's parked levels every ladderRetryInterval, backing
// off while none settles, until none is parked, the lane is done, or its
// deadline or drift bound has passed.
func (e *Executor) retryQuote(lane *quoteLane) {
	ctx := context.Background()
	interval := ladderRetryInterval
	failures := map[int]int{}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-e.quoteLanes.stopped():
			lane.mu.Lock()
			lane.retrying = false
			lane.mu.Unlock()
			return
		case <-timer.C:
		}
		lane.ops.Lock()
		lane.mu.Lock()
		var parked []*quoteLevel
		for _, level := range lane.levels {
			if level.parked {
				parked = append(parked, level)
			}
		}
		intent, deadline := lane.intent, lane.deadline
		if len(parked) == 0 || lane.done.Load() || e.quoteLanes.isClosed() {
			lane.retrying = false
			lane.mu.Unlock()
			lane.ops.Unlock()
			return
		}
		lane.mu.Unlock()
		if status, over := e.ladderOver(intent, deadline); over {
			reason := "deadline"
			if status == store.IntentStatusSignalDrift {
				reason = "drift"
			}
			if err := e.cancelQuoteLevels(ctx, lane, parked, reason); err != nil {
				e.reportError(err)
			}
			lane.ops.Unlock()
			timer.Reset(0)
			continue
		}
		settled := e.repostQuoteLevels(ctx, lane, intent, parked, failures)
		lane.ops.Unlock()
		if settled > 0 {
			interval = ladderRetryInterval
		} else if interval *= 2; interval > maxLadderRetryInterval {
			interval = maxLadderRetryInterval
		}
		timer.Reset(interval)
	}
}

func (r *quoteRegistry) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// repostQuoteLevels reposts parked levels in one batch and reports how many
// settled. Called with ops held.
func (e *Executor) repostQuoteLevels(ctx context.Context, lane *quoteLane, intent protocol.ExecutionIntent, parked []*quoteLevel, failures map[int]int) int {
	type outcome struct {
		level   *quoteLevel
		order   store.SignedOrderRecord
		outcome repostOutcome
		err     error
	}
	results := make([]outcome, len(parked))
	var wg sync.WaitGroup
	members := e.newPlaceMembers(len(parked))
	for i, level := range parked {
		member := members[i]
		wg.Go(func() {
			defer member.leave()
			ctx := context.WithValue(context.WithValue(ctx, receivedAtKey{}, time.Now()), placeMemberKey{}, member)
			next, result, err := e.repostLevel(ctx, intent, level.order)
			results[i] = outcome{level, next, result, err}
		})
	}
	wg.Wait()
	settled := 0
	var retire []*quoteLevel
	lane.mu.Lock()
	for _, r := range results {
		var unresolved unresolvedSubmission
		switch {
		case r.outcome == repostParkedAgain:
			r.level.order = r.order
			failures[r.level.sequence] = 0
		case r.outcome == repostSettled && r.err == nil && statemachine.IsTerminal(r.order.State):
			// Ended elsewhere (cancelled by the lane scan, say): no longer working.
			if lane.levels[r.level.cents] == r.level {
				delete(lane.levels, r.level.cents)
			}
			settled++
		case r.outcome == repostSettled && (r.err == nil || errors.As(r.err, &unresolved)):
			// Resting, filled, or unknown and left to the reconciler.
			r.level.parked = false
			settled++
		case r.outcome == repostSettled:
			// Refused for another reason: no longer working.
			if lane.levels[r.level.cents] == r.level {
				delete(lane.levels, r.level.cents)
			}
			settled++
			e.reportError(fmt.Errorf("quote level %s/%d: %w", intent.IntentID, r.level.sequence, r.err))
		case r.outcome == repostLater:
			if r.err != nil {
				e.reportError(fmt.Errorf("quote level %s/%d: %w", intent.IntentID, r.level.sequence, r.err))
			}
			if failures[r.level.sequence]++; failures[r.level.sequence] >= maxRepostFailures {
				retire = append(retire, r.level)
			}
		}
	}
	lane.mu.Unlock()
	if len(retire) > 0 {
		if err := e.cancelQuoteLevels(ctx, lane, retire, "requote"); err != nil {
			e.reportError(err)
		}
	}
	return settled
}

// checkQuotes is the quote lanes' share of the deadline pass: a lane past its
// deadline, or whose market drifted past its bound, has every level
// cancelled. It never waits for an exchange call: each lane's cancels run on
// their own goroutine, one sweep per lane at a time.
func (e *Executor) checkQuotes(ctx context.Context) {
	for _, lane := range e.quoteLanes.snapshot() {
		reason := e.quoteSweepReason(lane)
		if reason == "" || !lane.sweeping.CompareAndSwap(false, true) {
			continue
		}
		if !e.quoteLanes.goRun(func() {
			defer lane.sweeping.Store(false)
			lane.ops.Lock()
			defer lane.ops.Unlock()
			// Decide again under ops: a request applied meanwhile may have
			// moved the deadline or the signal price.
			switch reason := e.quoteSweepReason(lane); reason {
			case "":
			case "requote":
				lane.mu.Lock()
				stuck := slices.Clone(lane.stuck)
				for _, level := range lane.levels {
					if level.cancelling {
						stuck = append(stuck, level)
					}
				}
				lane.mu.Unlock()
				if err := e.cancelQuoteLevels(ctx, lane, stuck, reason); err != nil {
					e.reportError(err)
				}
			default:
				e.endQuoteLevels(ctx, lane, reason)
			}
		}) {
			lane.sweeping.Store(false)
		}
	}
}

// quoteSweepReason says why a lane's levels must come down now, or "".
func (e *Executor) quoteSweepReason(lane *quoteLane) string {
	lane.mu.Lock()
	working := len(lane.levels) + len(lane.stuck)
	retryCancel := len(lane.stuck) > 0
	for _, level := range lane.levels {
		retryCancel = retryCancel || level.cancelling
	}
	intent, deadline := lane.intent, lane.deadline
	lane.mu.Unlock()
	if working == 0 {
		return ""
	}
	if lane.done.Load() {
		return "fill"
	}
	if status, over := e.ladderOver(intent, deadline); over {
		if status == store.IntentStatusSignalDrift {
			return "drift"
		}
		return "deadline"
	}
	if retryCancel {
		return "requote"
	}
	return ""
}

// QuoteOrderObserved is the account feed's and the reconciler's hook into
// quote lanes (accountfeed.QuoteFillHook).
func (e *Executor) QuoteOrderObserved(ctx context.Context, intent store.OrderIntentRecord, order store.SignedOrderRecord, filled bool) {
	e.quoteOrderSeen(ctx, mapping.ExecutionIntent(intent), order, filled)
}

// quoteOrderSeen handles one observation of a quote level. A level that ended
// unfilled stops counting as working. The first fill ends the lane: done is
// set here, before any call, then the other levels are cancelled in one batch
// and the result is published, both off the caller's goroutine.
func (e *Executor) quoteOrderSeen(ctx context.Context, intent protocol.ExecutionIntent, order store.SignedOrderRecord, filled bool) {
	lane := e.quoteLanes.lane(quoteKeyOf(intent.UniqueTag, intent.ConditionID, intent.TokenID))
	if !filled {
		if statemachine.IsTerminal(order.State) {
			lane.mu.Lock()
			for cents, level := range lane.levels {
				if level.sequence == order.ChildSequence {
					delete(lane.levels, cents)
				}
			}
			lane.mu.Unlock()
		}
		return
	}
	if !lane.done.CompareAndSwap(false, true) {
		return
	}
	seenAt := time.Now()
	lane.mu.Lock()
	lane.filledAt = seenAt
	if lane.intent.IntentID == "" {
		lane.intent = intent
	}
	lane.mu.Unlock()
	e.quoteLanes.bindIntent(lane, intent.IntentID)
	ctx = context.WithoutCancel(ctx)
	e.quoteLanes.goRun(func() {
		// Fast path: cancel what is known to be working now, without waiting
		// for an apply or a repost in progress to finish.
		e.endQuoteLevels(ctx, lane, "fill")
		if e.quoteMetrics.FillToCancel != nil {
			e.quoteMetrics.FillToCancel(time.Since(seenAt))
		}
		// Then once more under ops, for anything a running apply posted, and
		// for the levels of a lane not loaded since a restart.
		lane.ops.Lock()
		if err := e.loadQuoteLane(ctx, lane); err != nil {
			e.reportError(err)
		}
		e.endQuoteLevels(ctx, lane, "fill")
		lane.ops.Unlock()
	})
	e.quoteLanes.goRun(func() {
		marked, err := e.store.MarkQuoteDone(ctx, lane.key, intent.IntentID, order.ChildSequence)
		if err != nil {
			e.reportError(err)
			return
		}
		if !marked {
			return // ended before a restart; its result was owed then
		}
		if err := e.publishQuoteResult(ctx, lane.key, intent.IntentID, order.ChildSequence); err != nil {
			e.reportError(err)
		}
	})
}

// publishQuoteResult publishes a lane's one result when it is owed, and only
// then marks it published: a crash in between publishes it again after the
// restart, rather than never. Within this process, one publish per lane runs
// at a time.
func (e *Executor) publishQuoteResult(ctx context.Context, key store.QuoteLaneKey, intentID string, sequence int) error {
	r := &e.quoteLanes
	r.mu.Lock()
	if r.publishing[key] {
		r.mu.Unlock()
		return nil
	}
	if r.publishing == nil {
		r.publishing = map[store.QuoteLaneKey]bool{}
	}
	r.publishing[key] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.publishing, key)
		r.mu.Unlock()
	}()
	// Re-read under the guard: another publish may have just finished.
	lane, found, err := e.store.QuoteLane(ctx, key)
	if err != nil {
		return err
	}
	if !found || lane.ResultState != store.QuoteResultOwed {
		return nil
	}
	result, err := e.quoteResult(ctx, intentID, sequence)
	if err == nil {
		err = protocol.PublishExecutionQuoteResult(e.publish, result)
	}
	if err != nil {
		return fmt.Errorf("publish quote result for %v: %w", key, err)
	}
	if _, err := e.store.MoveQuoteResult(ctx, key, store.QuoteResultPublished, store.QuoteResultOwed); err != nil {
		return fmt.Errorf("mark quote result for %v published: %w", key, err)
	}
	return nil
}

func (e *Executor) quoteResult(ctx context.Context, intentID string, sequence int) (protocol.ExecutionQuoteResult, error) {
	record, err := e.store.Intent(ctx, intentID)
	if err != nil {
		return protocol.ExecutionQuoteResult{}, fmt.Errorf("load quote intent: %w", err)
	}
	order, err := e.store.OrderByIntent(ctx, intentID, sequence)
	if err != nil {
		return protocol.ExecutionQuoteResult{}, fmt.Errorf("load filled quote level: %w", err)
	}
	requestID, err := e.store.QuoteLevelRequest(ctx, intentID, sequence)
	if err != nil {
		return protocol.ExecutionQuoteResult{}, err
	}
	if requestID == "" {
		requestID = e.quoteLevelRequest(intentID, sequence)
	}
	shares := order.MatchedShares
	if counter, ok := e.store.(interface {
		FilledShares(context.Context, string) (string, error)
	}); ok && order.ExchangeOrderID != "" {
		if recorded, err := counter.FilledShares(ctx, order.ExchangeOrderID); err == nil && decimal.Compare(recorded, shares) > 0 {
			shares = recorded
		}
	}
	filled, _ := decimal.NonNegativeFloat(shares)
	// A post-only buy only ever fills as the maker, at its own price, so
	// before the trade has landed the level's price is the fill's.
	averageText := accountfeed.AveragePrice(ctx, e.store, order)
	if averageText == "" && filled > 0 {
		averageText = order.Price
	}
	average, _ := decimal.Price(averageText)
	price := order.Price
	if cents, ok := wholeCents(order.Price); ok {
		price = centsPrice(cents)
	}
	return protocol.ExecutionQuoteResult{
		RequestID: requestID, UniqueTag: record.UniqueTag, Strategy: record.Strategy,
		ConditionID: record.ConditionID, TokenID: record.TokenID, Outcome: record.Outcome, Side: protocol.SideBuy,
		Price: price, FilledShares: filled, AveragePrice: average, OccurredAt: e.now(),
	}, nil
}

// quoteLevelRequest is the request a level was posted by, from memory.
func (e *Executor) quoteLevelRequest(intentID string, sequence int) string {
	e.quoteLanes.mu.Lock()
	lane := e.quoteLanes.byIntent[intentID]
	e.quoteLanes.mu.Unlock()
	if lane == nil {
		return ""
	}
	lane.mu.Lock()
	defer lane.mu.Unlock()
	for _, level := range lane.levels {
		if level.sequence == sequence {
			return level.requestID
		}
	}
	return lane.intent.RequestID
}

// WithdrawQuote cancels every working level of a quote lane, for a FORCE_CLOSE
// on it. It does not end the lane. It waits for the cancels.
func (e *Executor) WithdrawQuote(ctx context.Context, tag, conditionID, tokenID string) {
	e.quoteLanes.mu.Lock()
	lane := e.quoteLanes.lanes[quoteKeyOf(tag, conditionID, tokenID)]
	e.quoteLanes.mu.Unlock()
	if lane == nil {
		return // levels of a lane not loaded here are found by the lane scan
	}
	lane.ops.Lock()
	defer lane.ops.Unlock()
	e.endQuoteLevels(ctx, lane, "force_close")
}

// recoverQuotes is the recovery pass's share for quote lanes: lanes with
// levels still open are adopted, so the deadline keeps applying after a
// restart, and an owed result is published.
func (e *Executor) recoverQuotes(ctx context.Context, orders []store.SignedOrderRecord) error {
	var errs error
	seen := map[string]bool{}
	for _, order := range orders {
		if seen[order.IntentID] || e.quoteLanes.hasIntent(order.IntentID) {
			continue
		}
		seen[order.IntentID] = true
		record, found, err := e.store.QuoteLaneByIntent(ctx, order.IntentID)
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		if !found {
			continue
		}
		lane := e.quoteLanes.lane(record.Key)
		lane.ops.Lock()
		lane.mu.Lock()
		loaded := lane.loaded
		lane.mu.Unlock()
		if !loaded {
			errs = errors.Join(errs, e.restoreQuoteLane(ctx, lane, record, true))
		} else {
			e.quoteLanes.bindIntent(lane, order.IntentID)
		}
		lane.ops.Unlock()
	}
	e.pruneQuoteLanes(e.now().UTC())
	owed, err := e.store.OwedQuoteResults(ctx)
	if err != nil {
		return errors.Join(errs, err)
	}
	for _, lane := range owed {
		errs = errors.Join(errs, e.publishQuoteResult(ctx, lane.Key, lane.IntentID, lane.FilledSequence))
	}
	return errs
}

// quoteLaneIdle is how long after its deadline a lane with nothing working is
// kept in memory. A lane forgotten here is read back from the store on its
// next request, done flag and seq included.
const quoteLaneIdle = 10 * time.Minute

// pruneQuoteLanes forgets lanes with nothing working, nothing waiting and a
// deadline long past, so the deadline pass does not walk every market ever
// quoted.
func (e *Executor) pruneQuoteLanes(now time.Time) {
	r := &e.quoteLanes
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, lane := range r.lanes {
		if !lane.ops.TryLock() {
			continue
		}
		lane.mu.Lock()
		idle := len(lane.levels) == 0 && len(lane.stuck) == 0 && lane.pending == nil && !lane.draining && !lane.retrying &&
			!lane.sweeping.Load() && lane.deadline.Add(quoteLaneIdle).Before(now)
		intentID := lane.intent.IntentID
		if idle {
			lane.pruned = true
		}
		lane.mu.Unlock()
		lane.ops.Unlock()
		if !idle {
			continue
		}
		delete(r.lanes, key)
		if intentID != "" && r.byIntent[intentID] == lane {
			delete(r.byIntent, intentID)
		}
	}
}

// isQuoteOrder reports whether an order belongs to a quote lane known here.
// Such orders are the lane's to cancel, never the generic watcher's.
func (e *Executor) isQuoteOrder(intentID string) bool { return e.quoteLanes.hasIntent(intentID) }
