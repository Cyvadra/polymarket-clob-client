package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/mapping"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor/tactics"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// watchInterval is how often resting orders are checked against their
// deadline and drift bound. An entry with a 3s timeout chasing a moving book
// cannot wait a whole second past either.
const watchInterval = 50 * time.Millisecond

// cancelRetryWindow is how long the watcher keeps re-sending a cancel the
// exchange refuses, once per watchInterval. Polymarket's order endpoints
// allow well over 200 requests a second, so sixty cancels in three seconds is
// far inside the limit. A refused cancel is often an order that has already
// filled or been cancelled, whose account-feed update has not landed yet;
// after the window the order is left to the reconciler.
const cancelRetryWindow = 3 * time.Second

// watchedOrder is a resting order the watcher may cancel, with everything the
// check needs held in memory so a pass reads quotes and the clock, not the
// database.
type watchedOrder struct {
	intent   protocol.ExecutionIntent
	deadline time.Time // zero when the intent has none
	drift    tactics.Drift
	// cancelFrom is when the watcher first tried to cancel the order; zero
	// until a deadline or drift breach is seen.
	cancelFrom time.Time
	// gaveUp is set once cancelRetryWindow passes. The entry stays, skipped,
	// until the order leaves the open set, so adoptOpenOrders does not start
	// a fresh window on it every second.
	gaveUp bool
}

// watchOrder starts watching a resting order. createdAt is when the intent
// was stored, from which complete_within_ms runs. An order with neither a
// deadline nor a drift bound has nothing to be cancelled for.
func (e *Executor) watchOrder(intent protocol.ExecutionIntent, childSequence int, createdAt time.Time) {
	watched := &watchedOrder{intent: intent, deadline: intent.ExpiresAt}
	if within := intent.Policy.CompleteWithinMillis; within > 0 && !createdAt.IsZero() {
		if completeBy := createdAt.Add(time.Duration(within) * time.Millisecond); watched.deadline.IsZero() || completeBy.Before(watched.deadline) {
			watched.deadline = completeBy
		}
	}
	// Only an open's strategy child is cancelled for drift. A bound that does
	// not parse was refused at validation; it reads as none.
	if intent.Kind == protocol.IntentOpen && childSequence == store.StrategyChildSequence {
		watched.drift, _ = tactics.ParseDrift(intent)
	}
	if watched.deadline.IsZero() && !watched.drift.Bounded() {
		return
	}
	key := orderKey{intent.IntentID, childSequence}
	e.watchMu.Lock()
	defer e.watchMu.Unlock()
	if e.watched == nil {
		e.watched = make(map[orderKey]*watchedOrder)
	}
	if _, ok := e.watched[key]; !ok {
		e.watched[key] = watched
	}
}

func (e *Executor) isWatched(key orderKey) bool {
	e.watchMu.Lock()
	defer e.watchMu.Unlock()
	_, ok := e.watched[key]
	return ok
}

func (e *Executor) unwatch(key orderKey) {
	e.watchMu.Lock()
	defer e.watchMu.Unlock()
	delete(e.watched, key)
}

// checkWatched cancels every watched order past its deadline or drifted past
// its bound. Deciding that touches only memory; the database is read only for
// an order that is due a cancel, to learn whether it is still open and to
// walk its state from the current revision.
func (e *Executor) checkWatched(ctx context.Context) error {
	now := e.now().UTC()
	type due struct {
		key     orderKey
		watched *watchedOrder
		drifted bool
	}
	var pending []due
	e.watchMu.Lock()
	for key, watched := range e.watched {
		if watched.gaveUp {
			continue
		}
		expired := !watched.deadline.IsZero() && !watched.deadline.After(now)
		drifted := false
		if !expired && watched.cancelFrom.IsZero() && watched.drift.Bounded() && e.quotes != nil {
			quote, hasQuote := e.quotes.Get(watched.intent.ConditionID)
			_, drifted = tactics.DriftBreach(watched.intent, watched.drift, quote, hasQuote, now)
		}
		if expired || drifted || !watched.cancelFrom.IsZero() {
			pending = append(pending, due{key, watched, drifted})
		}
	}
	e.watchMu.Unlock()

	var errs error
	for _, d := range pending {
		if err := e.cancelWatched(ctx, d.key, d.watched, d.drifted, now); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	return errs
}

// cancelWatched makes one attempt to cancel a watched order. The order stops
// being watched once the store no longer shows it cancellable (filled,
// cancelled, or its cancel accepted), or once cancelRetryWindow passes.
func (e *Executor) cancelWatched(ctx context.Context, key orderKey, watched *watchedOrder, drifted bool, now time.Time) error {
	order, err := e.store.OrderByIntent(ctx, key.intentID, key.child)
	if errors.Is(err, store.ErrNotFound) {
		e.unwatch(key)
		return nil
	}
	if err != nil {
		return fmt.Errorf("load watched order %s/%d: %w", key.intentID, key.child, err)
	}
	switch order.State {
	case statemachine.StateLive, statemachine.StatePartiallyFilled, statemachine.StateCancelRequested:
	default:
		e.unwatch(key)
		return nil
	}
	if watched.cancelFrom.IsZero() {
		watched.cancelFrom = now
	} else if now.Sub(watched.cancelFrom) >= cancelRetryWindow {
		e.watchMu.Lock()
		watched.gaveUp = true
		e.watchMu.Unlock()
		return fmt.Errorf("cancel order %s still open after %s of attempts; leaving it to the reconciler", order.ExchangeOrderID, cancelRetryWindow)
	}
	reason := cancelRequested
	// The drift mark goes on before the cancel is sent, since the exchange
	// may confirm the cancel on the account stream before the call returns.
	// An order another path already asked to cancel is not marked, nor one
	// the store no longer shows resting: it filled after the read above, and
	// the next pass drops it.
	if drifted && order.State != statemachine.StateCancelRequested {
		marked, err := e.store.MarkIntentDrifted(ctx, key.intentID, key.child)
		if err != nil {
			return fmt.Errorf("mark intent %s drifted: %w", key.intentID, err)
		}
		if !marked {
			return nil
		}
		reason = cancelRequested + ": " + protocol.ReasonSignalDrift
	}
	// A refused cancel is retried on the next pass rather than reported: it
	// is usually an order that has just filled.
	_ = e.cancelOpenOrder(ctx, watched.intent.Policy, order, reason)
	return nil
}

// adoptOpenOrders watches resting orders the executor did not place in this
// process: those open before a restart, and those the reconciler found live
// after an unknown submit. It reads the intent only for an order not already
// watched, and drops orders the watcher gave up on once they are no longer
// open.
func (e *Executor) adoptOpenOrders(ctx context.Context, orders []store.SignedOrderRecord) error {
	open := make(map[orderKey]struct{}, len(orders))
	for _, order := range orders {
		open[orderKey{order.IntentID, order.ChildSequence}] = struct{}{}
	}
	e.watchMu.Lock()
	for key, watched := range e.watched {
		if _, ok := open[key]; watched.gaveUp && !ok {
			delete(e.watched, key)
		}
	}
	e.watchMu.Unlock()
	var errs error
	for _, order := range orders {
		if order.ExchangeOrderID == "" || (order.State != statemachine.StateLive && order.State != statemachine.StatePartiallyFilled && order.State != statemachine.StateCancelRequested) {
			continue
		}
		if e.isWatched(orderKey{order.IntentID, order.ChildSequence}) {
			continue
		}
		intent, err := e.store.Intent(ctx, order.IntentID)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("load intent %s to watch: %w", order.IntentID, err))
			continue
		}
		e.watchOrder(mapping.ExecutionIntent(intent), order.ChildSequence, intent.CreatedAt)
	}
	return errs
}
