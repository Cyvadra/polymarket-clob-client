// Package executor persists and submits one child order for each execution intent.
package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/mapping"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type CLOB interface {
	CreateOrder(context.Context, clobclient.UserOrder) (clobclient.SignedOrderV2, error)
	SubmitSignedOrder(context.Context, clobclient.SignedOrderV2, clobclient.OrderType, bool) (*clobclient.OrderResponse, error)
	CancelOrder(context.Context, string) error
	TickSize(context.Context, string) (float64, error)
	MinOrderSize(context.Context, string) (float64, error)
}

type QuoteProvider interface {
	Get(string) (marketquotes.Snapshot, bool)
}

// repository is the durable state the executor needs: the execution lifecycle
// plus read access to current positions (used by cancel-triggered force closes
// and future event-end force closes).
type repository interface {
	store.ExecutionStore
	store.PositionStore
	store.PositionReconciler
	// LanePosition reads one lane's position row; found is false when the
	// wallet has none. Every close checks its lane, and a close for another
	// wallet's strategy checks it to learn the lane is not held here, so it
	// must not cost a read of every position.
	LanePosition(ctx context.Context, conditionID, tokenID, uniqueTag string) (position store.PositionRecord, found bool, err error)
	// MarkIntentCanceling records why executiond is about to cancel an
	// intent's order (store.IntentStatusSignalDrift or
	// store.IntentStatusDeadline), only while the order is still resting, so
	// it never overwrites a fill that has just landed.
	MarkIntentCanceling(ctx context.Context, intentID string, childSequence int, status string) (marked bool, err error)
}

type Executor struct {
	store  repository
	clob   CLOB
	quotes QuoteProvider
	// books is the top of each market's maintained order book, from
	// pmm.market.levels, when the collector publishes it; nil otherwise.
	books   QuoteProvider
	now     func() time.Time
	onError func(error)
	publish protocol.ExecutionEventPublisher
	sizer   EntrySizer
	gate    OpenGate
	// onSubmit sees how long each order spent between its request arriving
	// and its POST starting: sizing, locking, persistence and signing.
	onSubmit func(intent protocol.ExecutionIntent, prePost time.Duration)

	// laneRevisions holds each lane's position revision as of the previous
	// maintenance pass, so the pass can tell a position that is still filling
	// from one that has come to rest. It is a cache of observations, not state
	// the trading path depends on: losing it across a restart costs one extra
	// tick before a close is resized.
	laneMu        sync.Mutex
	laneRevisions map[laneKey]int64
	// residualSweeps is when each lane's sub-minimum residual was last offered
	// to the book, so a size the exchange keeps refusing is not retried on
	// every tick.
	residualSweeps map[laneKey]time.Time

	// watched holds every resting order the executor may need to cancel for
	// its deadline or drift bound, checked every watchInterval (watch.go).
	watchMu sync.Mutex
	watched map[orderKey]*watchedOrder
}

// orderKey names one child order of an intent.
type orderKey struct {
	intentID string
	child    int
}

// cancelRequested is the reason a cancel records on its CANCEL_REQUESTED
// event; a drift cancel appends its code.
const cancelRequested = "cancellation requested"

func New(repository repository, clob CLOB, now func() time.Time) (*Executor, error) {
	if repository == nil || clob == nil {
		return nil, fmt.Errorf("repository and CLOB client are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Executor{store: repository, clob: clob, now: now}, nil
}

// SetEntrySizer enables opens sized by target_equity_fraction. Without one,
// such opens are rejected.
func (e *Executor) SetEntrySizer(sizer EntrySizer) {
	e.sizer = sizer
}

// OpenGate can hold back new positions, for every strategy or for one;
// closes are never gated.
type OpenGate interface {
	Suspended(strategy string) (bool, string)
}

// SetOpenGate rejects opens while gate reports trading suspended.
func (e *Executor) SetOpenGate(gate OpenGate) {
	e.gate = gate
}

func (e *Executor) SetQuoteProvider(provider QuoteProvider) {
	e.quotes = provider
}

// SetBookProvider gives the executor the top of each market's maintained
// order book (pmm.market.levels). pmm.market.quotes moves on book frames and
// on prints alike, and a print away from the book moves its mid without the
// book having moved; the drift check reads this book first when it is fresh,
// so an entry is not refused for a print the book never saw.
func (e *Executor) SetBookProvider(provider QuoteProvider) {
	e.books = provider
}

func (e *Executor) Init(context.Context) error { return nil }

// Run drives the executor's periodic passes on separate goroutines: the
// deadline and drift watcher every watchInterval, recovery of open orders
// and close maintenance every second.
//
// Deadline enforcement must not share a goroutine with close maintenance.
// maintainCloses makes one blocking exchange call per lane, so its pass takes
// as long as the lanes it walks; run in series, it starved cancelExpired and
// stretched the effective deadline check from 1s to 60s over nine days of
// uptime, leaving orders resting well past their expires_at.
func (e *Executor) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		e.loop(ctx, "deadline", watchInterval, e.checkWatched)
	}()
	go func() {
		defer wg.Done()
		e.loop(ctx, "open order recovery", time.Second, e.recoverOpenOrders)
	}()
	go func() {
		defer wg.Done()
		e.loop(ctx, "close maintenance", time.Second, e.maintainCloses)
	}()
	wg.Wait()
	return nil
}

// overrunFactor is how many times its own interval a pass may take before the
// loop reports it. A pass occasionally running long is ordinary; one taking
// ten intervals is no longer running at the frequency it was written for, and
// on a shared goroutine it would be starving whatever runs beside it. That is
// how the deadline check drifted from 1s to 60s unnoticed over nine days.
const overrunFactor = 10

// errorRepeatInterval is how often a loop reports a pass error identical to
// the last one it reported.
const errorRepeatInterval = time.Second

// loop runs pass on every tick until ctx is done, reporting a pass that takes
// far longer than its interval. A pass that overruns simply starts again on
// the next tick; ticks are dropped rather than queued, so a slow pass never
// builds a backlog.
func (e *Executor) loop(ctx context.Context, name string, interval time.Duration, pass func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	threshold := time.Duration(overrunFactor) * interval
	overrunning := false
	var lastErr string
	var lastErrAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			started := e.now()
			err := pass(ctx)
			elapsed := e.now().Sub(started)
			// A pass failing the same way every tick (the database down) is
			// reported once per errorRepeatInterval, not once per tick.
			if err != nil && (err.Error() != lastErr || started.Sub(lastErrAt) >= errorRepeatInterval) {
				e.reportError(err)
				lastErr, lastErrAt = err.Error(), started
			}
			switch {
			case elapsed >= threshold && !overrunning:
				overrunning = true
				e.reportError(fmt.Errorf("%s pass took %s, over %s for a %s interval: it is no longer running at its intended frequency", name, elapsed.Round(time.Millisecond), threshold, interval))
			case elapsed < threshold && overrunning:
				overrunning = false
				e.reportError(fmt.Errorf("%s pass recovered, back to %s for a %s interval", name, elapsed.Round(time.Millisecond), interval))
			}
		}
	}
}

// recoverOpenOrders resumes orders persisted as signed but never sent, and
// hands the watcher any resting order it is not yet watching.
func (e *Executor) recoverOpenOrders(ctx context.Context) error {
	orders, err := e.store.OpenOrders(ctx)
	if err != nil {
		return fmt.Errorf("load open orders for recovery: %w", err)
	}
	resumeErr := e.adoptOpenOrders(ctx, orders)
	for _, order := range orders {
		if order.State != statemachine.StateSigned {
			continue
		}
		intent, err := e.store.Intent(ctx, order.IntentID)
		if err != nil {
			resumeErr = errors.Join(resumeErr, fmt.Errorf("load signed intent %s: %w", order.IntentID, err))
			continue
		}
		if err := e.store.WithIntentLock(ctx, intent.IntentID, func(ctx context.Context) error {
			return e.submitSignedOrder(ctx, mapping.ExecutionIntent(intent), order)
		}); err != nil {
			resumeErr = errors.Join(resumeErr, fmt.Errorf("resume signed order %s/%d: %w", order.IntentID, order.ChildSequence, err))
		}
	}
	return resumeErr
}
func (e *Executor) Close(context.Context) error { return nil }

// SetSubmitLatencyHook reports, per order, the time from the request arriving
// to the POST starting. Set it before the executor handles requests.
func (e *Executor) SetSubmitLatencyHook(hook func(intent protocol.ExecutionIntent, prePost time.Duration)) {
	e.onSubmit = hook
}

type receivedAtKey struct{}

// intentCreatedAtKey carries the created_at an intent was stored with to
// the submit that places its order.
type intentCreatedAtKey struct{}

// withReceivedAt stamps when a request arrived, keeping an earlier stamp so
// an open that is sized before Execute is timed from its true start.
func withReceivedAt(ctx context.Context) context.Context {
	if _, ok := ctx.Value(receivedAtKey{}).(time.Time); ok {
		return ctx
	}
	return context.WithValue(ctx, receivedAtKey{}, time.Now())
}

func (e *Executor) SetErrorHandler(handler func(error)) {
	e.onError = handler
}

func (e *Executor) SetEventPublisher(publisher protocol.ExecutionEventPublisher) {
	e.publish = publisher
}

func (e *Executor) publishTransition(order store.SignedOrderRecord, intent protocol.ExecutionIntent, reason string) {
	if err := protocol.PublishExecutionOrderEvent(e.publish, order.ExchangeOrderID, string(order.State), order.IntentID, intent.UniqueTag, intent.Strategy, order.MatchedShares, reason, e.now()); err != nil && e.onError != nil {
		e.onError(fmt.Errorf("publish order transition: %w", err))
	}
}

// cancelOpenOrder cancels a single still-open exchange order, walking the
// cancel state chain and tolerating state conflicts when another path already
// observed the cancellation. Orders without an exchange order ID or in an
// uncancellable state are left untouched. reason is recorded on the
// CANCEL_REQUESTED event.
func (e *Executor) cancelOpenOrder(ctx context.Context, policy protocol.ExecutionPolicy, order store.SignedOrderRecord, reason string) error {
	if order.ExchangeOrderID == "" {
		return nil
	}
	switch order.State {
	case statemachine.StateLive, statemachine.StatePartiallyFilled, statemachine.StateCancelRequested:
	default:
		return nil
	}
	if order.State != statemachine.StateCancelRequested {
		updated, err := e.store.TransitionOrder(ctx, order, statemachine.EventCancelRequested, order.MatchedShares, order.ExchangeOrderID, reason)
		if err != nil {
			if err != store.ErrConflict {
				return fmt.Errorf("mark order %s cancellation requested: %w", order.ExchangeOrderID, err)
			}
			return nil
		}
		order = updated
	}
	cancelCtx, cancel := context.WithTimeout(ctx, cancelTimeout(policy))
	err := e.clob.CancelOrder(cancelCtx, order.ExchangeOrderID)
	cancel()
	if err != nil {
		return fmt.Errorf("cancel order %s: %w", order.ExchangeOrderID, err)
	}
	if _, err := e.store.TransitionOrder(ctx, order, statemachine.EventCancelAccepted, order.MatchedShares, order.ExchangeOrderID, "cancellation accepted"); err != nil && err != store.ErrConflict {
		return fmt.Errorf("mark order %s cancellation pending: %w", order.ExchangeOrderID, err)
	}
	return nil
}

func cancelTimeout(policy protocol.ExecutionPolicy) time.Duration {
	if policy.CancelTimeoutMillis > 0 {
		return time.Duration(policy.CancelTimeoutMillis) * time.Millisecond
	}
	return 5 * time.Second
}
