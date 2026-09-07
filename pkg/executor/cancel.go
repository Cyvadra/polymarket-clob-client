package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/mapping"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

const (
	// forceCloseChildSequence is the child sequence reserved for the internal
	// force-close sell appended to an intent. The strategy-facing child always
	// uses store.StrategyChildSequence, so an intent carries at most one
	// force-close child in the current runtime.
	forceCloseChildSequence = store.StrategyChildSequence + 1

	// forceClosePrice is the giveaway taker price used to exit a position that
	// is being abandoned. Selling at the minimum tick is intended to fill
	// whenever any liquidity exists. An unfilled exit is terminal and is never
	// retried; the position then awaits settlement.
	forceClosePrice = "0.01"
)

// Cancel abandons the intent and, when forced, exits the token's whole
// remaining available position. It cancels any still-open child order for the
// intent and submits an internal 0.01 SELL FAK child for the remaining
// exposure. The outcome is reported on execution.cancel.ack, which is terminal
// from the strategy's point of view even when the exit does not fill.
func (e *Executor) Cancel(ctx context.Context, req protocol.ExecutionCancelRequest) error {
	if err := validateCancelRequest(req); err != nil {
		e.publishCancelAck(protocol.ExecutionCancelAck{IntentID: req.IntentID, Status: protocol.CancelFailed, ReasonCode: "INVALID_CANCEL", Reason: err.Error(), OccurredAt: e.now()})
		return err
	}
	ack, err := e.cancelIntent(ctx, req.IntentID, req.Force)
	if err != nil {
		_, code, reason := reasonFor(err)
		e.publishCancelAck(protocol.ExecutionCancelAck{IntentID: req.IntentID, Status: protocol.CancelFailed, ReasonCode: code, Reason: reason, OccurredAt: e.now()})
		return err
	}
	e.publishCancelAck(ack)
	return nil
}

func validateCancelRequest(req protocol.ExecutionCancelRequest) error {
	if req.SchemaVersion != protocol.SchemaVersionV1 {
		return fmt.Errorf("cancel request requires schema_version %q", protocol.SchemaVersionV1)
	}
	if strings.TrimSpace(req.IntentID) == "" {
		return fmt.Errorf("cancel request intent_id is required")
	}
	return nil
}

func (e *Executor) cancelIntent(ctx context.Context, intentID string, force bool) (protocol.ExecutionCancelAck, error) {
	intent, err := e.store.Intent(ctx, intentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return protocol.ExecutionCancelAck{IntentID: intentID, Status: protocol.CancelNotFound, ReasonCode: "INTENT_NOT_FOUND", Reason: "execution intent not found", OccurredAt: e.now()}, nil
		}
		return protocol.ExecutionCancelAck{}, fmt.Errorf("load execution intent: %w", err)
	}
	var ack protocol.ExecutionCancelAck
	if err := e.store.WithIntentLock(ctx, intentID, func(ctx context.Context) error {
		ack, err = e.cancelIntentLocked(ctx, intent, force)
		return err
	}); err != nil {
		return protocol.ExecutionCancelAck{}, err
	}
	return ack, nil
}

func (e *Executor) cancelIntentLocked(ctx context.Context, intent store.OrderIntentRecord, force bool) (protocol.ExecutionCancelAck, error) {
	ack := protocol.ExecutionCancelAck{IntentID: intent.IntentID, OccurredAt: e.now()}
	canceled, err := e.cancelStrategyChild(ctx, intent)
	if err != nil {
		return ack, err
	}
	if canceled {
		ack.CanceledOrders = 1
	}
	if !force {
		ack.Status = protocol.CancelCanceled
		ack.Reason = cancelReason(canceled, "position preserved")
		return ack, nil
	}

	// A force-close child already exists: the exit is dispatched or terminal.
	// A previously unfilled exit is deliberately not retried.
	if _, err := e.store.OrderByIntent(ctx, intent.IntentID, forceCloseChildSequence); err == nil {
		ack.Status = protocol.CancelCompleted
		ack.Reason = "force close already dispatched for intent"
		return ack, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return ack, fmt.Errorf("load force close order: %w", err)
	}

	status, reason, err := e.forceClosePosition(ctx, intent)
	if err != nil {
		return ack, err
	}
	if status != "" {
		ack.Status, ack.Reason = status, reason
		return ack, nil
	}
	ack.Status = protocol.CancelCanceled
	ack.Reason = cancelReason(canceled, "no position to force close")
	return ack, nil
}

func cancelReason(canceled bool, suffix string) string {
	if canceled {
		return "open order canceled; " + suffix
	}
	return "no open order to cancel; " + suffix
}

// cancelStrategyChild stops the intent's strategy-facing child order and
// reports whether a live exchange order was actually canceled.
func (e *Executor) cancelStrategyChild(ctx context.Context, intent store.OrderIntentRecord) (bool, error) {
	order, err := e.store.OrderByIntent(ctx, intent.IntentID, store.StrategyChildSequence)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load child order for cancel: %w", err)
	}
	switch order.State {
	case statemachine.StateSigned:
		// Persisted but never submitted: there is no exchange order to cancel,
		// so mark it canceled to stop recovery from submitting it later.
		if _, err := e.store.TransitionOrder(ctx, order, statemachine.EventCancelObserved, order.MatchedShares, order.ExchangeOrderID, "cancel command before submission"); err != nil && err != store.ErrConflict {
			return false, fmt.Errorf("cancel unsubmitted child order: %w", err)
		}
		return false, nil
	case statemachine.StateLive, statemachine.StatePartiallyFilled, statemachine.StateCancelRequested:
		if err := e.cancelOpenOrder(ctx, intent, order); err != nil {
			return false, err
		}
		return true, nil
	default:
		// Terminal, or a cancel/submit outcome still in flight that the
		// exchange observation or reconciliation will settle.
		return false, nil
	}
}

// forceClosePosition sells the token's whole remaining available position at
// 0.01 FAK as an internal child of the intent. An empty status means there was
// nothing to close; a populated status is already terminal for the strategy.
func (e *Executor) forceClosePosition(ctx context.Context, intent store.OrderIntentRecord) (protocol.CancelAckStatus, string, error) {
	position, found, err := e.positionFor(ctx, intent.ConditionID, intent.TokenID)
	if err != nil {
		return "", "", err
	}
	if !found || !decimal.Positive(position.AvailableSize) {
		return "", "", nil
	}
	child := plannedChild{
		Sequence: forceCloseChildSequence, Shares: position.ActualShares, Price: forceClosePrice,
		TimeInForce: protocol.TimeInForceFAK, ReservationReason: "cancel command force close",
	}
	// Force close always sells, whatever side the original intent took.
	exit := mapping.ExecutionIntent(intent)
	exit.Side = protocol.SideSell
	exit.PostOnly = false

	if err := e.placeChild(ctx, exit, child); err != nil {
		var declared rejection
		if errors.As(err, &declared) {
			switch declared.code {
			case protocol.ReasonActiveSellReservation:
				return protocol.CancelActiveSellReservation, "another sell order is already active for this token", nil
			case protocol.ReasonNoPosition:
				return protocol.CancelNoPosition, "no available position to force close", nil
			}
		}
		return "", "", err
	}
	return protocol.CancelCompleted, "open orders canceled and position force close submitted at 0.01 FAK", nil
}

func (e *Executor) positionFor(ctx context.Context, conditionID, tokenID string) (store.PositionRecord, bool, error) {
	positions, err := e.store.PositionFeatures(ctx)
	if err != nil {
		return store.PositionRecord{}, false, fmt.Errorf("load positions: %w", err)
	}
	for _, candidate := range positions {
		if candidate.ConditionID == conditionID && candidate.TokenID == tokenID {
			return candidate, true, nil
		}
	}
	return store.PositionRecord{}, false, nil
}
