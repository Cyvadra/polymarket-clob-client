package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/mapping"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

const (
	// forceCloseChildSequence is the child sequence reserved for the internal
	// force-close sell appended to a close request.
	forceCloseChildSequence = store.StrategyChildSequence + 1
	forceClosePrice         = "0.01"
	closeRetryInterval      = 200 * time.Millisecond
	defaultCloseTimeout     = 5 * time.Second
	// settlementRetryWindow bounds how long a sell rejected for missing token
	// balance is retried. A position is booked from the CLOB match, but the
	// exchange only lets the tokens be sold once the trade settles on chain,
	// so a close sent right after a fill is refused for a few seconds. The
	// close subject is dispatched per lane (see internal/execution/nats), so a
	// lane retrying here does not hold up closes on other lanes.
	settlementRetryWindow = 30 * time.Second
)

// settlementRetryInterval spaces the retries of a sell refused for missing
// balance; each one signs and submits a fresh order, so it is slower than the
// reservation-conflict retry. A variable so tests need not wait.
var settlementRetryInterval = time.Second

// ExecuteClose replaces any still-pending close on the request's lane with a
// fresh close for the whole position. The lane advisory lock serializes
// concurrent close requests for the same lane, so a replacement never races a
// concurrent replacement, and (per the close contract) nothing is published
// until the replacement's strategy child resolves terminally.
func (e *Executor) ExecuteClose(ctx context.Context, req protocol.ExecutionCloseRequest) error {
	if err := validateCloseRequest(req); err != nil {
		return err
	}
	return e.store.WithIntentLock(ctx, closeLaneLockKey(req), func(ctx context.Context) error {
		return e.replaceCloseLane(ctx, req)
	})
}

func closeLaneLockKey(req protocol.ExecutionCloseRequest) string {
	return fmt.Sprintf("close:%s:%s:%s:%s", req.Strategy, req.UniqueTag, req.ConditionID, req.AssetID)
}

func validateCloseRequest(req protocol.ExecutionCloseRequest) error {
	if req.SchemaVersion != protocol.SchemaVersionV1 {
		return fmt.Errorf("close request requires schema_version %q", protocol.SchemaVersionV1)
	}
	if strings.TrimSpace(req.UniqueTag) == "" || strings.TrimSpace(req.Strategy) == "" || strings.TrimSpace(req.ConditionID) == "" || strings.TrimSpace(req.AssetID) == "" || strings.TrimSpace(req.Outcome) == "" {
		return fmt.Errorf("close request unique tag, strategy, condition ID, asset ID, and outcome are required")
	}
	if req.Mode != protocol.ExecutionCloseModeLimit && req.Mode != protocol.ExecutionCloseModeForce {
		return fmt.Errorf("invalid close mode %q", req.Mode)
	}
	if req.Mode == protocol.ExecutionCloseModeLimit {
		if _, err := decimal.Price(req.LimitPrice); err != nil {
			return fmt.Errorf("invalid limit price %q", req.LimitPrice)
		}
	}
	return nil
}

// replaceCloseLane runs one full close replacement under the lane lock: it
// retires every still-open strategy child on the lane (an unfilled open buy or
// a prior close) so the requested close becomes the only live order, then
// places it. Placement is retried for transient reservation conflicts while the
// stale children's cancellations settle. All DB writes happen on the lane-locked
// call path, so the replacement child is placed without a second intent-level
// lock transaction.
func (e *Executor) replaceCloseLane(ctx context.Context, req protocol.ExecutionCloseRequest) error {
	intent := e.closeIntent(req)
	// Fail a malformed close before disturbing the lane's live orders.
	if req.Mode == protocol.ExecutionCloseModeLimit {
		if err := validateIntentAt(intent, e.now().UTC()); err != nil {
			return err
		}
	}
	stale, err := e.laneActiveOrders(ctx, req.ConditionID, req.AssetID, req.UniqueTag)
	if err != nil {
		return err
	}
	for _, candidate := range stale {
		if !supersededBy(req.Mode, candidate) {
			continue
		}
		if err := e.replaceStaleChild(ctx, candidate); err != nil {
			return err
		}
	}
	timeout := closeRetryTimeout(req.Policy)
	decide := func(err error) (closeRetry, error) { return e.closeRetryDecision(ctx, intent, err) }
	// A retry after an exchange rejection needs a fresh intent: the rejected
	// child is terminal under the old intent ID and cannot be placed again.
	renew := func(fresh bool) {
		if fresh {
			intent.IntentID = newExecutionID()
		}
	}
	if req.Mode == protocol.ExecutionCloseModeForce {
		return e.retryClosePlacement(ctx, timeout, func(fresh bool) error {
			renew(fresh)
			return e.forceClosePosition(ctx, forceCloseIntentRecord(intent, e.now().UTC()))
		}, decide)
	}
	err = e.retryClosePlacement(ctx, timeout, func(fresh bool) error {
		renew(fresh)
		return e.persistAndSubmit(ctx, intent)
	}, decide)
	// A LIMIT_CLOSE that placement refused has no child to report its outcome,
	// so report the refusal itself. Other errors (a store failure, an unknown
	// submission outcome) are not published: the order may still be live and
	// will report through its own terminal result.
	var declared rejection
	if err != nil && errors.As(err, &declared) {
		e.publishCloseRejection(intent, declared)
	}
	return err
}

// supersededBy reports whether a fresh close in this mode retires the lane
// child. A FORCE_CLOSE retires everything: it exits before settlement and needs
// the position to stop moving. A LIMIT_CLOSE leaves a resting open BUY alone,
// so a partially filled entry keeps working; the close is resized to match by
// the maintenance pass as the position grows (see closemaintain.go). A prior
// close is always superseded, in either mode.
func supersededBy(mode protocol.ExecutionCloseMode, candidate closeableOrder) bool {
	if candidate.intent.Kind == store.IntentClose {
		return true
	}
	return mode == protocol.ExecutionCloseModeForce
}

func (e *Executor) publishCloseRejection(intent protocol.ExecutionIntent, declared rejection) {
	if err := protocol.PublishExecutionCloseResult(e.publish, protocol.ExecutionCloseResult{
		UniqueTag: intent.UniqueTag, ConditionID: intent.ConditionID, AssetID: intent.TokenID, Outcome: intent.Outcome,
		Side: protocol.SideSell, Status: protocol.ResultFailed, ReasonCode: declared.code, Reason: declared.reason,
		OccurredAt: e.now(),
	}); err != nil {
		e.reportError(fmt.Errorf("publish close rejection: %w", err))
	}
}

// replaceStaleChild retires one still-open strategy child that a fresh close
// supersedes. Close children are marked SUPERSEDED so their terminal results
// are suppressed; open (buy) children are merely canceled and keep reporting.
// The marker is written before the exchange cancel so no stale close result can
// slip through after it, and it is rolled back if the cancel fails so a close
// that is still live keeps its truthful result instead of vanishing silently.
func (e *Executor) replaceStaleChild(ctx context.Context, candidate closeableOrder) error {
	if candidate.intent.Kind != store.IntentClose {
		return e.cancelOpenOrder(ctx, candidate.intent, candidate.order)
	}
	prior := string(candidate.intent.Status)
	if err := e.store.UpdateIntentStatus(ctx, candidate.intent.IntentID, store.IntentStatusSuperseded); err != nil {
		return err
	}
	if err := e.cancelOpenOrder(ctx, candidate.intent, candidate.order); err != nil {
		if prior != "" {
			// The stale close is still live: undo the marker so it reports its
			// own terminal outcome instead of being silently suppressed.
			if restoreErr := e.store.UpdateIntentStatus(ctx, candidate.intent.IntentID, prior); restoreErr != nil && e.onError != nil {
				e.onError(fmt.Errorf("restore superseded close intent %s: %w", candidate.intent.IntentID, restoreErr))
			}
		}
		return err
	}
	e.releaseSupersededReservation(ctx, candidate)
	return nil
}

// releaseSupersededReservation frees the shares a cancelled close was holding,
// so the replacement can be sized for the whole position straight away.
//
// Without this the reservation lingers until the cancelled order is observed
// reaching a terminal state, which is a round trip away. The replacement would
// meanwhile be planned against an available_size that still excludes it and so
// would be sized too small — silently selling less than the position, or being
// refused for falling under the market minimum.
//
// The release is idempotent: if the observer gets there first, the reservation
// is already gone and the store reports it as not found.
func (e *Executor) releaseSupersededReservation(ctx context.Context, candidate closeableOrder) {
	id := reservationID(candidate.intent.IntentID, candidate.order.ChildSequence)
	if err := e.store.Release(ctx, id, "superseded by a replacement close"); err != nil && !errors.Is(err, store.ErrNotFound) {
		e.reportError(fmt.Errorf("release superseded close reservation %s: %w", id, err))
	}
}

func closeRetryTimeout(policy protocol.ExecutionPolicy) time.Duration {
	if policy.CancelReplaceTimeoutMillis > 0 {
		return time.Duration(policy.CancelReplaceTimeoutMillis) * time.Millisecond
	}
	return defaultCloseTimeout
}

func (e *Executor) closeIntent(req protocol.ExecutionCloseRequest) protocol.ExecutionIntent {
	// Force close exits at a nominal 0.01; limit close uses the caller's limit.
	price := forceClosePrice
	if req.Mode == protocol.ExecutionCloseModeLimit {
		price = req.LimitPrice
	}
	intent := protocol.ExecutionIntent{
		SchemaVersion: req.SchemaVersion,
		IntentID:      newExecutionID(),
		UniqueTag:     req.UniqueTag,
		Strategy:      req.Strategy,
		Kind:          protocol.IntentClose,
		ConditionID:   req.ConditionID,
		TokenID:       req.AssetID,
		Outcome:       req.Outcome,
		Side:          protocol.SideSell,
		LimitPrice:    price,
		TimeInForce:   req.TimeInForce,
		Policy:        req.Policy,
	}
	if req.Mode == protocol.ExecutionCloseModeForce {
		intent.TimeInForce = protocol.TimeInForceFAK
		intent.PostOnly = false
		intent.Policy.Style = protocol.ExecutionStyleTakerAggressive
	} else if intent.TimeInForce == "" {
		intent.TimeInForce = protocol.TimeInForceGTC
	}
	if req.Mode == protocol.ExecutionCloseModeLimit && intent.Policy.Style == "" {
		intent.Policy.Style = protocol.ExecutionStyleLimit
	}
	return intent
}

// laneActiveOrders returns every still-open strategy child on the close lane,
// whether an open (buy) child or a prior close (sell) child, so a replacement
// close supersedes and cancels all of them rather than only the first match.
func (e *Executor) laneActiveOrders(ctx context.Context, conditionID, tokenID, uniqueTag string) ([]closeableOrder, error) {
	orders, err := e.store.OpenOrders(ctx)
	if err != nil {
		return nil, fmt.Errorf("load open orders: %w", err)
	}
	var lane []closeableOrder
	for _, order := range orders {
		if order.ChildSequence != store.StrategyChildSequence {
			continue
		}
		intent, err := e.store.Intent(ctx, order.IntentID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("load order intent: %w", err)
		}
		if intent.ConditionID == conditionID && intent.TokenID == tokenID && intent.UniqueTag == uniqueTag {
			lane = append(lane, closeableOrder{intent: intent, order: order})
		}
	}
	return lane, nil
}

// closeRetry is the verdict on one failed close-placement attempt.
type closeRetry struct {
	// done means nothing is left to exit, so the close is complete.
	done bool
	// retry means the failure is transient and the attempt runs again.
	retry bool
	// fresh means the next attempt must use a new intent ID, because the
	// failed attempt left a terminal child under the current one.
	fresh bool
	// window extends the overall retry deadline to at least this long after
	// the first attempt; zero keeps the caller's timeout.
	window time.Duration
	// delay overrides closeRetryInterval before the next attempt.
	delay time.Duration
}

// retryClosePlacement runs one close-placement attempt until it succeeds, is
// decided done (nothing left to exit), or hits a permanent error, retrying up
// to timeout while the lane lock is held. Reservation conflicts are retried
// without persisting anything new. A sell the exchange refused for missing
// balance is retried with a fresh intent (see settlementRetryWindow); the
// refused order is terminal on the exchange, so nothing is submitted twice.
func (e *Executor) retryClosePlacement(ctx context.Context, timeout time.Duration, attempt func(fresh bool) error, decide func(error) (closeRetry, error)) error {
	if timeout <= 0 {
		timeout = defaultCloseTimeout
	}
	start := e.now().UTC()
	deadline := start.Add(timeout)
	fresh := false
	for {
		err := attempt(fresh)
		if err == nil {
			return nil
		}
		verdict, decisionErr := decide(err)
		if decisionErr != nil {
			return decisionErr
		}
		if verdict.done {
			return nil
		}
		if !verdict.retry {
			return err
		}
		if extended := start.Add(verdict.window); extended.After(deadline) {
			deadline = extended
		}
		if !e.now().UTC().Before(deadline) {
			return fmt.Errorf("close placement timed out: %w", err)
		}
		delay := closeRetryInterval
		if verdict.delay > 0 {
			delay = verdict.delay
		}
		if err := sleepContext(ctx, delay); err != nil {
			return err
		}
		fresh = verdict.fresh
	}
}

func (e *Executor) closeRetryDecision(ctx context.Context, intent protocol.ExecutionIntent, err error) (closeRetry, error) {
	var declared rejection
	if !errors.As(err, &declared) {
		return closeRetry{}, nil
	}
	var retry closeRetry
	switch {
	case declared.code == protocol.ReasonActiveSellReservation, declared.code == protocol.ReasonNoPosition:
		retry = closeRetry{retry: true}
	case declared.code == protocol.ReasonOrderRejected && isSizeMismatch(declared):
		// The order was well formed and only its size was wrong, so a retry
		// against a freshly read position is worth the settlement window. A
		// reported balance of 0 is the usual case: the fill has not settled on
		// chain yet and the wallet holds nothing to sell.
		if err := e.adoptExchangeBalance(ctx, intent, declared); err != nil {
			return closeRetry{}, err
		}
		retry = closeRetry{retry: true, fresh: true, window: settlementRetryWindow, delay: settlementRetryInterval}
	default:
		return closeRetry{}, nil
	}
	position, found, posErr := e.positionFor(ctx, intent.ConditionID, intent.TokenID, intent.UniqueTag)
	if posErr != nil {
		return closeRetry{}, posErr
	}
	if !found || !decimal.Positive(position.ActualShares) {
		return closeRetry{done: true}, nil
	}
	return retry, nil
}

// adoptExchangeBalance records the position the exchange reported in a size
// refusal, so the retry plans from the wallet's real holding instead of the
// local fill ledger that just proved wrong. A report the store declines to
// apply (it never raises a position) simply leaves the sizes as they were.
func (e *Executor) adoptExchangeBalance(ctx context.Context, intent protocol.ExecutionIntent, declared rejection) error {
	reported, ok := exchangeBalance(declared)
	if !ok {
		return nil
	}
	// A reported balance of zero is the unsettled case, not a smaller holding:
	// the fill is booked but the tokens have not reached the wallet yet.
	// Clamping the lane to zero there would read as "nothing left to exit" and
	// cancel the retry that is waiting for settlement.
	if !decimal.Positive(reported) {
		return nil
	}
	if err := e.store.ReconcilePositionSize(ctx, intent.ConditionID, intent.TokenID, intent.UniqueTag, reported); err != nil {
		return fmt.Errorf("adopt exchange-reported position %s: %w", reported, err)
	}
	return nil
}

func forceCloseIntentRecord(intent protocol.ExecutionIntent, now time.Time) store.OrderIntentRecord {
	return store.OrderIntentRecord{
		IntentID:    intent.IntentID,
		UniqueTag:   intent.UniqueTag,
		Strategy:    intent.Strategy,
		Kind:        store.IntentClose,
		ConditionID: intent.ConditionID,
		TokenID:     intent.TokenID,
		Outcome:     intent.Outcome,
		Side:        store.Side(intent.Side),
		LimitPrice:  intent.LimitPrice,
		TimeInForce: store.TimeInForce(intent.TimeInForce),
		PostOnly:    intent.PostOnly,
		Status:      statemachine.StateIntentReceived,
		CreatedAt:   now,
	}
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type closeableOrder struct {
	intent store.OrderIntentRecord
	order  store.SignedOrderRecord
}

// forceClosePosition dispatches a best-effort 0.01 FAK exit for the whole
// remaining position. The exit is an internal child (sequence 2) and never
// produces a close result; the strategy reconciles it from position.features.*.
// A vanished position is treated as completed; a competing active sell is left
// for the caller's retry loop to resolve. The parent CLOSE intent is persisted
// first so the exit order's orders.intent_id foreign key is satisfied; because
// the intent carries no strategy child it still never emits a close result.
func (e *Executor) forceClosePosition(ctx context.Context, intent store.OrderIntentRecord) error {
	position, found, err := e.positionFor(ctx, intent.ConditionID, intent.TokenID, intent.UniqueTag)
	if err != nil {
		return err
	}
	if !found || !decimal.Positive(position.ActualShares) {
		return nil
	}
	// The exit bypasses the planner, so it floors to share precision here: an
	// unfloored size is rejected by the signer, and the reservation is gated on
	// available_size, not actual_shares.
	shares, err := sellableShares(position)
	if err != nil {
		return err
	}
	shares, ok := decimal.FloorTo(shares, clobclient.SharePrecisionDigits(protocol.SideSell, protocol.TimeInForceFAK))
	if !ok {
		return rejection{code: protocol.ReasonNoPosition, reason: fmt.Sprintf("position %s rounds to zero at share precision", position.ActualShares)}
	}
	if _, err := e.store.InsertIntent(ctx, intent); err != nil {
		return fmt.Errorf("persist force-close intent: %w", err)
	}
	child := plannedChild{Sequence: forceCloseChildSequence, Shares: shares, Price: forceClosePrice, TimeInForce: protocol.TimeInForceFAK, ReservationReason: "force close"}
	exit := mapping.ExecutionIntent(intent)
	exit.Side = protocol.SideSell
	exit.PostOnly = false
	if err := e.placeChild(ctx, exit, child); err != nil {
		return err
	}
	return nil
}

func (e *Executor) positionFor(ctx context.Context, conditionID, tokenID, uniqueTag string) (store.PositionRecord, bool, error) {
	positions, err := e.store.PositionFeatures(ctx)
	if err != nil {
		return store.PositionRecord{}, false, fmt.Errorf("load positions: %w", err)
	}
	for _, candidate := range positions {
		if candidate.ConditionID == conditionID && candidate.TokenID == tokenID && candidate.UniqueTag == uniqueTag {
			return candidate, true, nil
		}
	}
	return store.PositionRecord{}, false, nil
}
