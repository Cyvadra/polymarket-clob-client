package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/mapping"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// laneKey identifies one position lane, matching the key the positions table
// and the close lane lock are built on.
type laneKey struct {
	conditionID string
	tokenID     string
	uniqueTag   string
}

func laneOf(conditionID, tokenID, uniqueTag string) laneKey {
	return laneKey{conditionID: conditionID, tokenID: tokenID, uniqueTag: uniqueTag}
}

// maintainCloses keeps each lane's resting limit close sized to the position it
// is meant to exit.
//
// A close is sized from the position at the instant it is placed. When the
// lane's open buy is still filling — which a LIMIT_CLOSE no longer cancels, see
// supersededBy — the position keeps growing underneath the resting close, and
// the surplus would be left behind at settlement. Re-placing the close for the
// whole current position is the only way to exit it, because an incremental
// close for the difference alone is usually below the market minimum and is
// refused.
//
// Two cases are handled per lane:
//
//   - a resting close that is now smaller than the position: cancel and replace
//     it at the current size, once the position stops moving;
//   - a position with no resting close, too small to place at all: wait for the
//     bid to reach the strategy's own limit price and take it with a FAK.
func (e *Executor) maintainCloses(ctx context.Context) error {
	orders, err := e.store.OpenOrders(ctx)
	if err != nil {
		return fmt.Errorf("load open orders for close maintenance: %w", err)
	}
	positions, err := e.store.PositionFeatures(ctx)
	if err != nil {
		return fmt.Errorf("load positions for close maintenance: %w", err)
	}
	resting, maintainErr := e.restingCloses(ctx, orders)
	settled := e.settledLanes(positions)
	for _, position := range positions {
		lane := laneOf(position.ConditionID, position.TokenID, position.UniqueTag)
		if !decimal.Positive(position.ActualShares) {
			continue
		}
		current, held := resting[lane]
		switch {
		case held:
			if err := e.resizeRestingClose(ctx, position, current, settled[lane]); err != nil {
				maintainErr = errors.Join(maintainErr, fmt.Errorf("resize close on lane %s: %w", lane.uniqueTag, err))
			}
		default:
			if err := e.sweepResidual(ctx, position); err != nil {
				maintainErr = errors.Join(maintainErr, fmt.Errorf("sweep residual on lane %s: %w", lane.uniqueTag, err))
			}
		}
	}
	return maintainErr
}

// restingClose is one lane's live strategy close child and the intent behind it.
type restingClose struct {
	intent store.OrderIntentRecord
	order  store.SignedOrderRecord
}

// restingCloses indexes every live strategy close child by lane. Force-close
// exits (child sequence 2) are excluded: they are FAK and never rest.
func (e *Executor) restingCloses(ctx context.Context, orders []store.SignedOrderRecord) (map[laneKey]restingClose, error) {
	var loadErr error
	resting := map[laneKey]restingClose{}
	for _, order := range orders {
		if order.ChildSequence != store.StrategyChildSequence {
			continue
		}
		if order.State != statemachine.StateLive && order.State != statemachine.StatePartiallyFilled {
			continue
		}
		intent, err := e.store.Intent(ctx, order.IntentID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			loadErr = errors.Join(loadErr, fmt.Errorf("load intent %s: %w", order.IntentID, err))
			continue
		}
		if intent.Kind != store.IntentClose || intent.Status == store.IntentStatusSuperseded {
			continue
		}
		resting[laneOf(intent.ConditionID, intent.TokenID, intent.UniqueTag)] = restingClose{intent: intent, order: order}
	}
	return resting, loadErr
}

// settledLanes reports which lanes did not move since the previous pass, and
// records the current revisions for the next one.
//
// A position that is still filling changes on every tick, and cancel-replacing
// the close on each change would churn the book and repeatedly expose the lane
// to a rejected replacement. Waiting for one quiet tick is what "monitor it
// until it is fully filled" amounts to in practice, without needing to know
// whether more fills are coming.
func (e *Executor) settledLanes(positions []store.PositionRecord) map[laneKey]bool {
	settled := map[laneKey]bool{}
	seen := make(map[laneKey]int64, len(positions))
	e.laneMu.Lock()
	defer e.laneMu.Unlock()
	for _, position := range positions {
		lane := laneOf(position.ConditionID, position.TokenID, position.UniqueTag)
		seen[lane] = position.SourceRevision
		previous, known := e.laneRevisions[lane]
		settled[lane] = known && previous == position.SourceRevision
	}
	e.laneRevisions = seen
	return settled
}

// resizeRestingClose replaces a close whose size has fallen behind the lane's
// position. The replacement covers the whole position rather than the shortfall
// because the shortfall alone is usually under the market minimum.
func (e *Executor) resizeRestingClose(ctx context.Context, position store.PositionRecord, resting restingClose, settled bool) error {
	if !settled {
		return nil
	}
	// What the resting order still covers is what it was signed for, less what
	// it has already sold.
	covered := resting.order.RequestedShares
	if decimal.Positive(resting.order.MatchedShares) {
		remaining, ok := decimal.SubString(covered, resting.order.MatchedShares)
		if !ok {
			return nil
		}
		covered = remaining
	}
	sellable, err := sellableShares(position)
	if err != nil {
		// The lane is fully reserved by this very order; nothing to resize.
		return nil
	}
	// The resting order holds its own size in reserve, so the sellable figure
	// excludes it. Add it back to compare like with like.
	exposed, ok := decimal.AddString(sellable, covered)
	if !ok {
		return nil
	}
	if decimal.Compare(exposed, covered) <= 0 {
		return nil
	}
	minimum, err := e.clob.MinOrderSize(ctx, position.TokenID)
	if err != nil {
		return fmt.Errorf("load minimum order size: %w", err)
	}
	if belowMinimum(exposed, minimum) {
		// Cancelling a placeable order in favour of an unplaceable one is
		// strictly worse; leave the lane as it is.
		return nil
	}
	return e.ExecuteClose(ctx, closeRequestFrom(resting.intent))
}

// sweepResidual disposes of a position left with no resting close. The size is
// typically a late fill that landed after the close was filled or cancelled,
// and it is usually below the market minimum, so a resting limit order cannot
// be placed for it at all.
//
// Rather than fail, the residual waits for the market to come to the strategy's
// own limit price and then takes it with a FAK. The check runs on the executor
// tick against the quote cache, which is fed roughly once a second — close
// enough for a residual that would otherwise simply be abandoned.
func (e *Executor) sweepResidual(ctx context.Context, position store.PositionRecord) error {
	if e.quotes == nil {
		return nil
	}
	sellable, err := sellableShares(position)
	if err != nil {
		return nil
	}
	minimum, err := e.clob.MinOrderSize(ctx, position.TokenID)
	if err != nil {
		return fmt.Errorf("load minimum order size: %w", err)
	}
	if !belowMinimum(sellable, minimum) {
		// Placeable as an ordinary close. Re-placing it here would retry once a
		// second forever on a lane whose close keeps being refused, publishing
		// a FAILED close result each time. The strategy owns that decision and
		// re-sends its own close; this pass only handles what the strategy
		// cannot place at all. Checking this before looking up the close intent
		// also keeps ordinary open positions off the database on every tick.
		return nil
	}
	intent, err := e.store.LatestLimitCloseIntent(ctx, position.ConditionID, position.TokenID, position.UniqueTag)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The lane was never asked to close; it is still an open position.
			return nil
		}
		return fmt.Errorf("load latest close intent: %w", err)
	}
	if !e.bidReached(position, intent.LimitPrice) {
		return nil
	}
	if !e.residualSweepDue(laneOf(position.ConditionID, position.TokenID, position.UniqueTag)) {
		return nil
	}
	return e.takeResidual(ctx, intent, sellable)
}

// residualSweepInterval spaces repeated attempts on the same residual. The
// exchange may refuse a sub-minimum size however it is sent, and retrying that
// every tick would hammer the API for shares worth cents. A variable so tests
// need not wait.
var residualSweepInterval = 30 * time.Second

// residualSweepDue reports whether enough time has passed to try this lane's
// residual again, and records the attempt.
func (e *Executor) residualSweepDue(lane laneKey) bool {
	now := e.now().UTC()
	e.laneMu.Lock()
	defer e.laneMu.Unlock()
	if last, ok := e.residualSweeps[lane]; ok && now.Sub(last) < residualSweepInterval {
		return false
	}
	if e.residualSweeps == nil {
		e.residualSweeps = map[laneKey]time.Time{}
	}
	e.residualSweeps[lane] = now
	return true
}

// bidReached reports whether the lane's best bid has come up to the close's
// limit price.
func (e *Executor) bidReached(position store.PositionRecord, limitPrice string) bool {
	snapshot, ok := e.quotes.Get(position.ConditionID)
	if !ok {
		return false
	}
	var bid float64
	switch strings.TrimSpace(position.TokenID) {
	case strings.TrimSpace(snapshot.Up.AssetID):
		bid = snapshot.Up.Bid
	case strings.TrimSpace(snapshot.Down.AssetID):
		bid = snapshot.Down.Bid
	default:
		return false
	}
	limit, err := decimal.Price(limitPrice)
	if err != nil {
		return false
	}
	return bid >= limit
}

// takeResidual sells the residual at the close's limit price with a FAK. It is
// placed as an internal child (sequence 2), like a force close, so it publishes
// no close result: the strategy reconciles the exit from position.features, and
// a sub-minimum residual the exchange still refuses must not read as a failed
// take-profit.
func (e *Executor) takeResidual(ctx context.Context, intent store.OrderIntentRecord, shares string) error {
	exit := mapping.ExecutionIntent(intent)
	exit.IntentID = newExecutionID()
	exit.Side = protocol.SideSell
	exit.PostOnly = false
	exit.TimeInForce = protocol.TimeInForceFAK
	exit.Policy.Style = protocol.ExecutionStyleTakerAggressive

	record := forceCloseIntentRecord(exit, e.now().UTC())
	record.TimeInForce = store.TimeInForce(protocol.TimeInForceFAK)
	if _, err := e.store.InsertIntent(ctx, record); err != nil {
		return fmt.Errorf("persist residual exit intent: %w", err)
	}
	child := plannedChild{
		Sequence: forceCloseChildSequence, Shares: shares, Price: intent.LimitPrice,
		TimeInForce: protocol.TimeInForceFAK, ReservationReason: "residual take-profit",
	}
	if err := e.placeChild(ctx, exit, child); err != nil {
		// The exchange may refuse a residual under its minimum however it is
		// sent. Report it and leave the shares for the pre-settlement force
		// close rather than failing the pass.
		e.reportError(fmt.Errorf("take residual %s on lane %s: %w", shares, intent.UniqueTag, err))
	}
	return nil
}

// closeRequestFrom rebuilds the close request behind a persisted intent, so a
// replacement runs through the same ExecuteClose path — lane lock, supersede,
// cancel, retry — as the original.
func closeRequestFrom(intent store.OrderIntentRecord) protocol.ExecutionCloseRequest {
	wire := mapping.ExecutionIntent(intent)
	return protocol.ExecutionCloseRequest{
		SchemaVersion: protocol.SchemaVersionV1,
		UniqueTag:     intent.UniqueTag,
		Strategy:      intent.Strategy,
		ConditionID:   intent.ConditionID,
		AssetID:       intent.TokenID,
		Outcome:       intent.Outcome,
		Mode:          protocol.ExecutionCloseModeLimit,
		LimitPrice:    intent.LimitPrice,
		TimeInForce:   protocol.TimeInForce(intent.TimeInForce),
		Policy:        wire.Policy,
	}
}

func belowMinimum(shares string, minimum float64) bool {
	if minimum <= 0 {
		return false
	}
	parsed, err := decimal.PositiveFloat(shares)
	if err != nil {
		return true
	}
	return parsed+1e-9 < minimum
}
