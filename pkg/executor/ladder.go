package executor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/mapping"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/accountfeed"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor/tactics"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// A multi-level intent (policy.style MULTI_LEVEL_LIMIT) is one OPEN BUY posted
// as a post-only GTC child at every cent of bottom_price..limit_price. Child
// sequence 1 is the lowest price, so the highest sequence is the level closest
// to the limit.
//
// Its life, from the request to the one aggregated result:
//
//  1. placeLadder plans every level, then posts them concurrently. A level
//     whose post-only order would cross the book is not a failure: it parks in
//     RETRY_PENDING with its reservation held.
//  2. retryLadder reposts each parked level every ladderRetryInterval, with a
//     fresh signature, until it rests, fills or is refused for another reason.
//  3. The ladder ends on CANCEL_OPEN (or a force close), on its deadline, or on
//     signal drift. Resting levels are cancelled on the exchange, parked ones
//     are cancelled locally.
//  4. When every child is terminal, PublishLadderResult publishes the result.
//
// The lifecycle lives in the intent's ladder_phase, never in its status: the
// status follows each child's state and would overwrite a marker.

const (
	// ladderRetryInterval is how often a parked level is reposted.
	ladderRetryInterval = 25 * time.Millisecond
	// maxLadderRetryInterval caps the backoff while every parked level keeps
	// crossing: a level at or through the ask cannot rest until the book
	// moves, and reposting it every 25 ms only spends rate limit.
	maxLadderRetryInterval = time.Second
	// ladderPhaseAttempts bounds the retries of the PLACING to ACTIVE move.
	ladderPhaseAttempts = 5
	// DefaultMaxLadderLevels bounds the levels of one intent unless
	// SetMaxLadderLevels says otherwise: each level reserves target_usd.
	DefaultMaxLadderLevels = 20
	// ladderConcurrency bounds the posts of one ladder in flight at once.
	ladderConcurrency = 8
	// maxRepostFailures is how many consecutive errors before a signing or
	// store failure retires a parked level instead of retrying it.
	maxRepostFailures = 5
	// centsPerUnit converts a price to the cent grid.
	centsPerUnit = 100
)

// ladderSpec is a validated ladder range, in whole cents.
type ladderSpec struct{ bottom, limit int }

func (l ladderSpec) levels() int { return l.limit - l.bottom + 1 }

// sequence is the stable child sequence of the level at cents.
func (l ladderSpec) sequence(cents int) int { return cents - l.bottom + 1 }

func centsPrice(cents int) string {
	return fmt.Sprintf("%d.%02d", cents/centsPerUnit, cents%centsPerUnit)
}

// wholeCents reads a price that must sit exactly on the cent grid.
func wholeCents(text string) (int, bool) {
	value, ok := decimal.Rat(text)
	if !ok {
		return 0, false
	}
	value.Mul(value, big.NewRat(centsPerUnit, 1))
	if !value.IsInt() || !value.Num().IsInt64() {
		return 0, false
	}
	return int(value.Num().Int64()), true
}

// parseLadder validates a multi-level intent and returns its range. It is the
// one place that defines what a well-formed ladder is: intent validation and
// planning both call it.
func parseLadder(intent protocol.ExecutionIntent) (ladderSpec, error) {
	if intent.Side != protocol.SideBuy || intent.TimeInForce != protocol.TimeInForceGTC || !intent.PostOnly {
		return ladderSpec{}, invalid("multi-level limit must be a post-only GTC buy")
	}
	if intent.ScalingMode != protocol.ScalingModeFlat {
		return ladderSpec{}, invalid("multi-level limit supports scaling_mode %q only, not %q", protocol.ScalingModeFlat, intent.ScalingMode)
	}
	bottom, bottomOK := wholeCents(intent.BottomPrice)
	limit, limitOK := wholeCents(intent.LimitPrice)
	if !bottomOK || !limitOK || bottom < 1 || limit >= centsPerUnit || bottom > limit {
		return ladderSpec{}, invalid("multi-level limit needs whole-cent prices with 0 < bottom_price <= limit_price < 1, got %q..%q", intent.BottomPrice, intent.LimitPrice)
	}
	return ladderSpec{bottom: bottom, limit: limit}, nil
}

// checkLadderGrid refuses a market whose tick does not divide a cent: a level
// at a whole cent would be off its price grid and the signer would refuse it.
func checkLadderGrid(tick float64) error {
	const cent = 1.0 / centsPerUnit
	ratio := cent / tick
	if tick <= 0 || tick > cent+1e-9 || math.Abs(ratio-math.Round(ratio)) > 1e-6 {
		return reject(protocol.ReasonUnplannable, "market tick size %v does not divide the one-cent ladder step", tick)
	}
	return nil
}

// SetMaxLadderLevels bounds the levels one multi-level intent may span; n <= 0
// restores DefaultMaxLadderLevels. Set it before the executor handles requests.
func (e *Executor) SetMaxLadderLevels(n int) { e.maxLadderLevels = n }

func (e *Executor) ladderLevelLimit() int {
	if e.maxLadderLevels > 0 {
		return e.maxLadderLevels
	}
	return DefaultMaxLadderLevels
}

// planLadder plans one child per level, highest price first so that, if the
// wallet's exposure runs out part way, the levels nearest the market are the
// ones placed. A level under the market's minimum order size is skipped; the
// ladder fails only when no level is left.
func (e *Executor) planLadder(ctx context.Context, intent protocol.ExecutionIntent, spec ladderSpec) ([]plannedChild, error) {
	if spec.levels() > e.ladderLevelLimit() {
		return nil, invalid("multi-level limit spans %d levels, over the limit of %d", spec.levels(), e.ladderLevelLimit())
	}
	market, err := e.marketRules(ctx, intent.TokenID)
	if err != nil {
		return nil, err
	}
	if err := checkLadderGrid(market.TickSize); err != nil {
		return nil, err
	}
	now := e.now().UTC()
	if err := e.checkOpenDrift(intent, now); err != nil {
		return nil, err
	}
	level := intent
	level.Policy.Style = protocol.ExecutionStyleLimit
	children := make([]plannedChild, 0, spec.levels())
	var tooSmall error
	for cents := spec.limit; cents >= spec.bottom; cents-- {
		level.LimitPrice = centsPrice(cents)
		decision, err := tactics.Plan(tactics.Request{Intent: level, Market: market, Now: now})
		switch {
		case err == nil:
			children = append(children, plannedChild{
				Sequence: spec.sequence(cents), Shares: decision.Shares, Price: decision.Price,
				PostOnly: decision.PostOnly, TimeInForce: decision.TimeInForce,
				ReservationReason: "multi-level limit child accepted",
			})
		case tactics.BelowMinOrderSize(err):
			tooSmall = err
		default:
			return nil, rejection{code: protocol.ReasonUnplannable, reason: err.Error(), cause: err}
		}
	}
	if len(children) == 0 {
		return nil, rejection{code: protocol.ReasonBelowMinOrderSize, reason: tooSmall.Error(), cause: tooSmall}
	}
	return children, nil
}

// parkedError reports that a level's post-only order crossed the book and was
// parked for another try. order is the RETRY_PENDING row to retry from.
type parkedError struct{ order store.SignedOrderRecord }

func (e *parkedError) Error() string {
	return fmt.Sprintf("level %d crossed the book and is parked for retry", e.order.ChildSequence)
}

// placeLadder is prepareAndSubmit for a multi-level intent. It returns once
// every level has been posted or parked; the retries continue in the
// background, so the lane worker is free for the CANCEL_OPEN that ends them.
func (e *Executor) placeLadder(ctx context.Context, intent protocol.ExecutionIntent) error {
	spec, err := parseLadder(intent)
	if err != nil {
		return err
	}
	children, err := e.planLadder(ctx, intent, spec)
	if err != nil {
		return err
	}
	// Register before marking PLACING: whoever sees the phase (adoption after
	// a restart, the recovery pass) then also sees the run and leaves it be.
	stop, run, err := e.ladders.begin(context.WithoutCancel(ctx), intent.IntentID)
	if err != nil {
		return err
	}
	if moved, err := e.store.MoveLadderPhase(ctx, intent.IntentID, store.LadderPlacing, store.LadderNone); err != nil || !moved {
		run.finish()
		if err == nil {
			err = fmt.Errorf("multi-level intent %s was already started", intent.IntentID)
		}
		return err
	}
	// The posts outlive the request: once a level is reserved it is placed.
	// Only the decision to start another is cancellable (stop). The entry's
	// free-cash claim stays held until ExecuteOpen returns, not until the
	// first level has reserved, so a second entry cannot be sized from cash
	// the rest of the ladder is about to take.
	postCtx := withoutCashHold(context.WithoutCancel(ctx))
	parked, placed, placeErr := e.postLevels(stop, postCtx, intent, children)
	// A CANCEL_OPEN taken before the first level was saved found nothing to
	// stop; the post gate is the only one that saw it. It cancelled each POST
	// in flight, and the ladder must now end as though stopLadder had run.
	dropped := postGateDropped(ctx)
	// Placement is over however it ended. The phase moves while the run is
	// still registered, so no other observer publishes in between.
	next := store.LadderActive
	switch {
	case placed == 0 && len(parked) == 0:
		// Nothing rests and nothing is parked: the caller reports the failure
		// (ExecuteOpen publishes the rejection), so the ladder must not report
		// it as well. The claim keeps any later observer from doing so.
		next = store.LadderPublished
	case dropped:
		next = store.LadderStopping
	}
	e.moveLadderPhaseRetrying(postCtx, intent.IntentID, next, store.LadderPlacing, store.LadderStopping)
	if next == store.LadderPublished {
		run.finish()
		return placeErr
	}
	if len(parked) > 0 && stop.Err() == nil && !dropped {
		createdAt, _ := ctx.Value(intentCreatedAtKey{}).(time.Time)
		go e.retryLadder(stop, postCtx, run, intent, ladderDeadline(intent, createdAt), parked)
	} else {
		// Stopped or dropped: nothing will repost the parked levels. A cancel
		// that fails here is left to the recovery pass (sweepParkedLadders).
		for _, order := range parked {
			if err := e.cancelParkedOrder(postCtx, intent, order, cancelRequested); err != nil {
				e.reportError(err)
			}
		}
		run.finish()
	}
	e.publishLadder(ctx, intent.IntentID, "multi-level placement ended")
	return nil
}

// moveLadderPhaseRetrying moves a phase past a transient store failure: a
// ladder left in PLACING is never published, so it must not stay there.
func (e *Executor) moveLadderPhaseRetrying(ctx context.Context, intentID string, to store.LadderPhase, from ...store.LadderPhase) {
	for attempt := 1; ; attempt++ {
		_, err := e.store.MoveLadderPhase(ctx, intentID, to, from...)
		if err == nil {
			return
		}
		if attempt >= ladderPhaseAttempts {
			e.reportError(fmt.Errorf("mark multi-level intent %s %s: %w", intentID, to, err))
			return
		}
		time.Sleep(time.Duration(attempt) * ladderRetryInterval * 4)
	}
}

// postGateDropped reports whether the post gate saw a CANCEL_OPEN for this
// open. It is only asked once placement has posted, past the point of no
// return the gate guards.
func postGateDropped(ctx context.Context) bool {
	gate, ok := ctx.Value(postGateKey{}).(func() bool)
	return ok && gate()
}

// postLevels posts the planned levels concurrently, ladderConcurrency at a
// time, and reports the parked ones and how many were placed. A level that
// fails on its own does not stop the others; only when none was placed is the
// first failure the ladder's. Each level is reserved here, one at a time in
// plan order (highest price first), before its post starts: when exposure
// runs out part way, the levels nearest the market are the ones placed.
func (e *Executor) postLevels(stop, ctx context.Context, intent protocol.ExecutionIntent, children []plannedChild) (map[int]store.SignedOrderRecord, int, error) {
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		parked   = map[int]store.SignedOrderRecord{}
		placed   int
		firstErr error
	)
	fail := func(sequence int, err error) {
		if firstErr == nil {
			firstErr = err
		} else {
			e.reportError(fmt.Errorf("multi-level intent %s level %d: %w", intent.IntentID, sequence, err))
		}
	}
	slots := make(chan struct{}, ladderConcurrency)
	for _, child := range children {
		if stop.Err() != nil {
			break
		}
		// Wait for a slot before reserving, so a reservation is never held
		// by a level still queued behind the posts in flight.
		slots <- struct{}{}
		if err := e.reserveChild(ctx, intent, child, reservationID(intent.IntentID, child.Sequence)); err != nil {
			<-slots
			mu.Lock()
			fail(child.Sequence, err)
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			err := e.placeReservedChild(ctx, intent, child)
			var wait *parkedError
			var unresolved unresolvedSubmission
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil, errors.As(err, &unresolved):
				// An unknown outcome is a live order for now: the
				// reconciler decides it, and it must not fail the ladder.
				placed++
			case errors.As(err, &wait):
				parked[child.Sequence] = wait.order
			default:
				fail(child.Sequence, err)
			}
		}()
	}
	wg.Wait()
	return parked, placed, firstErr
}

// ladderDeadline is the earliest moment a parked level must stop retrying:
// expires_at, or complete_within_ms from when the intent was stored. A level
// that rests carries the same bound through the watcher.
func ladderDeadline(intent protocol.ExecutionIntent, createdAt time.Time) time.Time {
	deadline := intent.ExpiresAt
	if within := intent.Policy.CompleteWithinMillis; within > 0 && !createdAt.IsZero() {
		if at := createdAt.Add(time.Duration(within) * time.Millisecond); deadline.IsZero() || at.Before(deadline) {
			deadline = at
		}
	}
	return deadline
}

// ladderOver reports whether a parked ladder must stop retrying, and the
// intent status that records why: its deadline passed or the market drifted
// past its bound. The watcher applies the same two rules to resting levels.
func (e *Executor) ladderOver(intent protocol.ExecutionIntent, deadline time.Time) (string, bool) {
	now := e.now().UTC()
	if !deadline.IsZero() && !now.Before(deadline) {
		return store.IntentStatusDeadline, true
	}
	if drift, _ := tactics.ParseDrift(intent); drift.Bounded() {
		quote, ok := e.driftQuote(intent, now)
		if _, breached := tactics.DriftBreach(intent, drift, quote, ok, now); breached {
			return store.IntentStatusSignalDrift, true
		}
	}
	return "", false
}

// retryLadder reposts the parked levels every ladderRetryInterval until none
// is left, the ladder is stopped, or it is over. It owns the parked rows: no
// other path moves a RETRY_PENDING order of a registered ladder, because a
// cancel stops the ladder, and waits for this goroutine, before it looks.
func (e *Executor) retryLadder(stop, ctx context.Context, run *ladderRun, intent protocol.ExecutionIntent, deadline time.Time, parked map[int]store.SignedOrderRecord) {
	defer run.finish()
	interval := ladderRetryInterval
	timer := time.NewTimer(interval)
	defer timer.Stop()
	failures := map[int]int{}
	for len(parked) > 0 {
		select {
		case <-stop.Done():
			return
		case <-timer.C:
		}
		if status, over := e.ladderOver(intent, deadline); over {
			e.retireParked(ctx, intent, parked, status)
			break
		}
		before := len(parked)
		e.repostParked(stop, ctx, intent, parked, failures)
		// Back off while nothing settles; any progress resets the pace.
		if len(parked) < before {
			interval = ladderRetryInterval
		} else if interval *= 2; interval > maxLadderRetryInterval {
			interval = maxLadderRetryInterval
		}
		timer.Reset(interval)
	}
	e.publishLadder(ctx, intent.IntentID, "multi-level retries ended")
}

type repostOutcome int

const (
	// repostParkedAgain: crossed again, the row is parked with a new revision.
	repostParkedAgain repostOutcome = iota
	// repostSettled: the level rests, filled, was refused or is unknown; it
	// is no longer this loop's to retry.
	repostSettled
	// repostLater: nothing was sent (signing or the store failed); the same
	// row is still parked and the next tick tries again.
	repostLater
)

func (e *Executor) repostParked(stop, ctx context.Context, intent protocol.ExecutionIntent, parked map[int]store.SignedOrderRecord, failures map[int]int) {
	type result struct {
		sequence int
		order    store.SignedOrderRecord
		outcome  repostOutcome
		err      error
	}
	results := make(chan result, len(parked))
	slots := make(chan struct{}, ladderConcurrency)
	var wg sync.WaitGroup
	for sequence, order := range parked {
		if stop.Err() != nil {
			break
		}
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			// Each repost is timed from now, not from the request.
			next, outcome, err := e.repostLevel(context.WithValue(ctx, receivedAtKey{}, time.Now()), intent, order)
			results <- result{sequence, next, outcome, err}
		}()
	}
	wg.Wait()
	close(results)
	for r := range results {
		if r.err != nil {
			e.reportError(fmt.Errorf("multi-level intent %s level %d: %w", intent.IntentID, r.sequence, r.err))
		}
		switch r.outcome {
		case repostParkedAgain:
			parked[r.sequence] = r.order
			failures[r.sequence] = 0
		case repostSettled:
			delete(parked, r.sequence)
		case repostLater:
			if failures[r.sequence]++; failures[r.sequence] >= maxRepostFailures {
				if err := e.cancelParkedOrder(ctx, intent, parked[r.sequence], "multi-level retry failed repeatedly"); err != nil {
					e.reportError(err)
				}
				delete(parked, r.sequence)
			}
		}
	}
}

// repostLevel signs a parked level afresh and posts it. A new signature, not
// the rejected payload, keeps the exchange from refusing the repost as a
// duplicate of the order it just rejected.
func (e *Executor) repostLevel(ctx context.Context, intent protocol.ExecutionIntent, order store.SignedOrderRecord) (store.SignedOrderRecord, repostOutcome, error) {
	child := plannedChild{Sequence: order.ChildSequence, Shares: order.RequestedShares, Price: order.Price, PostOnly: order.PostOnly, TimeInForce: protocol.TimeInForce(order.OrderType)}
	signed, err := e.signChild(ctx, intent, child)
	if err != nil {
		return order, repostLater, err
	}
	submitting, err := e.store.ResignRetryOrder(ctx, order, signed.payload, signed.hash, signed.salt, signed.order.OrderID, "retry post-only cross")
	switch {
	case errors.Is(err, store.ErrConflict):
		// Something else moved the order: it is no longer parked.
		return order, repostSettled, nil
	case err != nil:
		return order, repostLater, fmt.Errorf("re-sign parked order: %w", err)
	}
	err = e.sendOrder(ctx, intent, signed.order, child, submitting)
	var wait *parkedError
	switch {
	case err == nil:
		return submitting, repostSettled, nil
	case errors.As(err, &wait):
		return wait.order, repostParkedAgain, nil
	default:
		return submitting, repostSettled, err
	}
}

// retireParked ends the retries of a ladder that is over: the intent records
// why (so the result names it), then every parked level is cancelled locally.
func (e *Executor) retireParked(ctx context.Context, intent protocol.ExecutionIntent, parked map[int]store.SignedOrderRecord, status string) {
	if err := e.store.UpdateIntentStatus(ctx, intent.IntentID, status); err != nil {
		e.reportError(fmt.Errorf("mark multi-level intent %s %s: %w", intent.IntentID, status, err))
	}
	for sequence, order := range parked {
		if err := e.cancelParkedOrder(ctx, intent, order, cancelRequested+": "+status); err != nil {
			e.reportError(err)
		}
		delete(parked, sequence)
	}
}

// cancelParkedOrder cancels a RETRY_PENDING order. Nothing is on the exchange,
// so it only moves the row and frees the reservation. A conflict means the
// order already moved on and is somebody else's to cancel.
func (e *Executor) cancelParkedOrder(ctx context.Context, intent protocol.ExecutionIntent, order store.SignedOrderRecord, reason string) error {
	canceled, err := e.store.TransitionOrder(ctx, order, statemachine.EventCancelObserved, order.MatchedShares, order.ExchangeOrderID, reason)
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cancel parked order %s/%d: %w", order.IntentID, order.ChildSequence, err)
	}
	e.publishTransition(canceled, intent, reason)
	if err := e.store.Release(ctx, reservationID(order.IntentID, order.ChildSequence), reason); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("release parked order %s/%d: %w", order.IntentID, order.ChildSequence, err)
	}
	return nil
}

// publishLadder publishes the aggregated result if it is due.
func (e *Executor) publishLadder(ctx context.Context, intentID, reason string) {
	if err := accountfeed.PublishLadderResult(ctx, e.store, e.publish, intentID, reason, e.now()); err != nil {
		e.reportError(err)
	}
}

// isLadder reports whether an intent record is a multi-level intent. The
// account feed owns the definition, since it publishes the results.
var isLadder = accountfeed.IsLadderIntent

// stopLadder ends a ladder's retries before the caller cancels its orders:
// no level is reposted after it returns. It waits for the retry goroutine, so
// the caller reads rows that goroutine is no longer changing. Calling it for
// an intent that is not a ladder, or not running here, does nothing.
func (e *Executor) stopLadder(ctx context.Context, intent store.OrderIntentRecord) error {
	if !isLadder(intent) {
		return nil
	}
	e.ladders.stop(intent.IntentID)
	if _, err := e.store.MoveLadderPhase(ctx, intent.IntentID, store.LadderStopping, store.LadderPlacing, store.LadderActive); err != nil {
		return fmt.Errorf("mark multi-level intent %s stopping: %w", intent.IntentID, err)
	}
	return nil
}

// cancelLadderChild cancels one child of a stopped ladder from its current
// row: the row the lane scan read may predate the last retry.
func (e *Executor) cancelLadderChild(ctx context.Context, intent store.OrderIntentRecord, order store.SignedOrderRecord) error {
	fresh, err := e.store.OrderByIntent(ctx, order.IntentID, order.ChildSequence)
	if err != nil {
		return fmt.Errorf("reload ladder child %s/%d: %w", order.IntentID, order.ChildSequence, err)
	}
	wire := mapping.ExecutionIntent(intent)
	switch fresh.State {
	case statemachine.StateRetryPending:
		return e.cancelParkedOrder(ctx, wire, fresh, cancelRequested)
	case statemachine.StateSubmitting, statemachine.StateSubmitUnknown:
		return e.cancelSubmittingOrder(ctx, wire.Policy, fresh)
	}
	return e.cancelOpenOrder(ctx, wire.Policy, fresh, cancelRequested)
}

// cancelLadderLevels cancels the levels of one stopped ladder concurrently,
// ladderConcurrency at a time and the highest price first: one at a time, the
// last of 20 resting levels would wait out 19 cancel round trips, and the
// levels nearest the market are the ones most likely to fill meanwhile. Each
// call touches only its own row, so the cancels are independent.
func (e *Executor) cancelLadderLevels(ctx context.Context, intent store.OrderIntentRecord, orders []store.SignedOrderRecord) error {
	orders = slices.Clone(orders)
	slices.SortFunc(orders, func(a, b store.SignedOrderRecord) int { return b.ChildSequence - a.ChildSequence })
	var (
		mu   sync.Mutex
		errs error
		wg   sync.WaitGroup
	)
	slots := make(chan struct{}, ladderConcurrency)
	for _, order := range orders {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			if err := e.cancelLadderChild(ctx, intent, order); err != nil {
				mu.Lock()
				errs = errors.Join(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errs
}

// ladderRun is one ladder's placement and retries in this process.
type ladderRun struct {
	registry *ladderRegistry
	intentID string
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
}

// finish ends the run: it unregisters and releases whoever waits in stop.
func (r *ladderRun) finish() {
	r.once.Do(func() {
		r.cancel()
		r.registry.mu.Lock()
		if r.registry.runs[r.intentID] == r {
			delete(r.registry.runs, r.intentID)
		}
		r.registry.mu.Unlock()
		close(r.done)
	})
}

// ladderRegistry tracks the ladders running here, so a cancel can reach them.
type ladderRegistry struct {
	mu     sync.Mutex
	runs   map[string]*ladderRun
	closed bool
}

var errExecutorClosed = errors.New("executor is closed")

// begin registers a run and returns the context that is cancelled to stop it.
// The run ends when its owner calls finish.
func (r *ladderRegistry) begin(parent context.Context, intentID string) (context.Context, *ladderRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil, errExecutorClosed
	}
	if _, running := r.runs[intentID]; running {
		return nil, nil, fmt.Errorf("multi-level intent %s is already running", intentID)
	}
	ctx, cancel := context.WithCancel(parent)
	run := &ladderRun{registry: r, intentID: intentID, cancel: cancel, done: make(chan struct{})}
	if r.runs == nil {
		r.runs = map[string]*ladderRun{}
	}
	r.runs[intentID] = run
	return ctx, run, nil
}

func (r *ladderRegistry) has(intentID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.runs[intentID]
	return ok
}

// stop cancels a run and waits until its placement and retries have ended.
func (r *ladderRegistry) stop(intentID string) {
	r.mu.Lock()
	run := r.runs[intentID]
	r.mu.Unlock()
	if run != nil {
		run.cancel()
		<-run.done
	}
}

// closeAll stops every run and refuses new ones.
func (r *ladderRegistry) closeAll() {
	r.mu.Lock()
	r.closed = true
	runs := make([]*ladderRun, 0, len(r.runs))
	for _, run := range r.runs {
		runs = append(runs, run)
	}
	r.mu.Unlock()
	for _, run := range runs {
		run.cancel()
	}
	for _, run := range runs {
		<-run.done
	}
}

// publishPendingLadders publishes the result of every ladder whose children have
// all ended but whose result is still owed: the process died between the last
// child ending and the publish, or the publish failed and its claim was undone.
// Nothing else wakes such a ladder, since no child will change again.
func (e *Executor) publishPendingLadders(ctx context.Context) error {
	ids, err := e.store.PendingLadders(ctx)
	if err != nil {
		return fmt.Errorf("list pending ladders: %w", err)
	}
	var errs error
	for _, id := range ids {
		if e.ladders.has(id) {
			continue // still placing or retrying here; its own end publishes
		}
		if err := accountfeed.PublishLadderResult(ctx, e.store, e.publish, id, "multi-level result recovered", e.now()); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	return errs
}

// adoptLadders runs once after a restart. A ladder this process is not running
// lost its placement and its retries with the old one: its phase moves from
// PLACING to ACTIVE so its result can be published, including a ladder whose
// saved levels have all ended, and any level parked in RETRY_PENDING resumes
// retrying under the intent's deadline. A ladder that was already STOPPING
// is never resumed: its parked levels are cancelled instead.
func (e *Executor) adoptLadders(ctx context.Context, orders []store.SignedOrderRecord) error {
	if e.laddersAdopted.Load() {
		return nil
	}
	placing, err := e.store.PlacingLadders(ctx)
	if err != nil {
		return fmt.Errorf("list placing ladders: %w", err)
	}
	parked := map[string]map[int]store.SignedOrderRecord{}
	var intents []string
	note := func(intentID string) {
		if _, seen := parked[intentID]; !seen && !e.ladders.has(intentID) {
			parked[intentID] = map[int]store.SignedOrderRecord{}
			intents = append(intents, intentID)
		}
	}
	for _, order := range orders {
		note(order.IntentID)
		if level, ok := parked[order.IntentID]; ok && order.State == statemachine.StateRetryPending {
			level[order.ChildSequence] = order
		}
	}
	for _, intentID := range placing {
		note(intentID)
	}
	var errs error
	for _, intentID := range intents {
		record, err := e.store.Intent(ctx, intentID)
		if errors.Is(err, store.ErrNotFound) {
			continue // an order with no intent is nobody's ladder
		}
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("load intent %s to adopt: %w", intentID, err))
			continue
		}
		if !isLadder(record) {
			continue
		}
		if _, err := e.store.MoveLadderPhase(ctx, intentID, store.LadderActive, store.LadderPlacing); err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		if len(parked[intentID]) == 0 {
			continue
		}
		phase, err := e.store.LadderPhase(ctx, intentID)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("read phase of %s to adopt: %w", intentID, err))
			continue
		}
		intent := mapping.ExecutionIntent(record)
		if phase != store.LadderActive {
			// Stopped (or already published) before the restart: the cancel
			// that stopped it may not have reached every parked level.
			for _, order := range parked[intentID] {
				errs = errors.Join(errs, e.cancelParkedOrder(ctx, intent, order, cancelRequested))
			}
			continue
		}
		stop, run, err := e.ladders.begin(ctx, intentID)
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		// The stored creation time lets a level that finally rests be watched
		// for its deadline and drift, as it would have been on the live path.
		postCtx := context.WithValue(context.WithoutCancel(ctx), intentCreatedAtKey{}, record.CreatedAt)
		go e.retryLadder(stop, postCtx, run, intent, ladderDeadline(intent, record.CreatedAt), parked[intentID])
	}
	if errs == nil {
		e.laddersAdopted.Store(true)
	}
	return errs
}

// sweepParkedLadders cancels the parked levels nothing will retry: those of a
// ladder not running here once adoption is done. Only a cancel that failed
// (a store error in cancelParkedOrder) leaves one behind, holding its
// reservation and the ladder's result.
func (e *Executor) sweepParkedLadders(ctx context.Context, orders []store.SignedOrderRecord) error {
	if !e.laddersAdopted.Load() {
		return nil // adoption still owns them
	}
	var errs error
	intents := map[string]protocol.ExecutionIntent{}
	for _, order := range orders {
		if order.State != statemachine.StateRetryPending || e.ladders.has(order.IntentID) {
			continue
		}
		intent, ok := intents[order.IntentID]
		if !ok {
			record, err := e.store.Intent(ctx, order.IntentID)
			if err != nil {
				errs = errors.Join(errs, fmt.Errorf("load intent %s to sweep: %w", order.IntentID, err))
				continue
			}
			intent = mapping.ExecutionIntent(record)
			intents[order.IntentID] = intent
		}
		// A stopper racing this cancel (cancelLadderChild) loses or wins the
		// row's revision; cancelParkedOrder treats the conflict as done.
		errs = errors.Join(errs, e.cancelParkedOrder(ctx, intent, order, "multi-level parked level left behind"))
	}
	return errs
}
