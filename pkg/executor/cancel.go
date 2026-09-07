package executor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

const (
	// forceCloseChildSequence is the child sequence reserved for the internal
	// force-close sell appended to an intent. The strategy-facing first child
	// always uses sequence one, so an intent carries at most one force-close
	// child in the current runtime.
	forceCloseChildSequence = 2

	// forceClosePrice is the giveaway taker price used to exit a position that
	// is being abandoned (cancel command or, later, event-end force close).
	// Selling at the minimum tick is intended to fill whenever any liquidity
	// exists. An unfilled 0.01 FAK is treated as terminal for tracking purposes
	// (the position awaits settlement) and is never retried.
	forceClosePrice = "0.01"
)

// Cancel abandons the intent and, when the intent opened a position, force
// closes the token's whole remaining available position. It cancels any
// still-open child order for the intent and submits an internal 0.01 SELL FAK
// child for the remaining exposure. The result is reported on
// execution.cancel.ack; the cancel acknowledgement is terminal from the
// strategy's point of view even when the force-close sell does not fill.
func (e *Executor) Cancel(ctx context.Context, req protocol.ExecutionCancelRequest) error {
	if err := validateCancelRequest(req); err != nil {
		e.publishCancelAck(protocol.ExecutionCancelAck{IntentID: req.IntentID, Status: protocol.CancelFailed, ReasonCode: "INVALID_CANCEL", Reason: err.Error(), OccurredAt: e.now()})
		return err
	}
	ack, err := e.cancelIntent(ctx, req.IntentID, req.Reason)
	if err != nil {
		e.publishCancelAck(protocol.ExecutionCancelAck{IntentID: req.IntentID, Status: protocol.CancelFailed, ReasonCode: "EXECUTION_FAILED", Reason: err.Error(), OccurredAt: e.now()})
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

func (e *Executor) cancelIntent(ctx context.Context, intentID, reason string) (protocol.ExecutionCancelAck, error) {
	intent, err := e.store.Intent(ctx, intentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return protocol.ExecutionCancelAck{IntentID: intentID, Status: protocol.CancelNotFound, ReasonCode: "INTENT_NOT_FOUND", Reason: "execution intent not found", OccurredAt: e.now()}, nil
		}
		return protocol.ExecutionCancelAck{}, fmt.Errorf("load execution intent: %w", err)
	}
	var ack protocol.ExecutionCancelAck
	err = e.store.WithIntentLock(ctx, intentID, func(ctx context.Context) error {
		ack, err = e.cancelIntentLocked(ctx, intent, reason)
		return err
	})
	if err != nil {
		return protocol.ExecutionCancelAck{}, err
	}
	return ack, nil
}

func (e *Executor) cancelIntentLocked(ctx context.Context, intent store.OrderIntentRecord, reason string) (protocol.ExecutionCancelAck, error) {
	ack := protocol.ExecutionCancelAck{IntentID: intent.IntentID, OccurredAt: e.now()}

	// Cancel the intent's strategy-facing child order when it is still open.
	order, orderErr := e.store.OrderByIntent(ctx, intent.IntentID, 1)
	if orderErr != nil && !errors.Is(orderErr, store.ErrNotFound) {
		return ack, fmt.Errorf("load child order for cancel: %w", orderErr)
	}
	if orderErr == nil {
		switch order.State {
		case statemachine.StateSigned:
			// Persisted but never submitted: there is no exchange order to
			// cancel, so mark it canceled before submission to stop recovery
			// from submitting it later.
			if _, err := e.store.TransitionOrder(ctx, order, statemachine.EventCancelObserved, order.MatchedShares, order.ExchangeOrderID, "cancel command before submission"); err != nil && err != store.ErrConflict {
				return ack, fmt.Errorf("cancel unsigned child order: %w", err)
			}
		case statemachine.StateLive, statemachine.StatePartiallyFilled, statemachine.StateCancelRequested:
			if err := e.cancelOpenOrder(ctx, intent, order); err != nil {
				return ack, err
			}
			ack.CanceledOrders = 1
		case statemachine.StateCancelPending, statemachine.StateSubmitting, statemachine.StateSubmitUnknown, statemachine.StateUnknownReconcile:
			// Cancellation already in flight or submit outcome unresolved; left
			// for the exchange observation or reconciliation to settle.
		default:
			// Terminal; nothing left to cancel.
		}
	}

	// A force-close child (sequence two) already exists for this intent: the
	// exit is dispatched or already terminal. Do not place another one; a
	// previously unfilled force close is deliberately not retried.
	if _, secondErr := e.store.OrderByIntent(ctx, intent.IntentID, forceCloseChildSequence); secondErr == nil {
		ack.Status = protocol.CancelCompleted
		ack.Reason = "force close already dispatched for intent"
		return ack, nil
	} else if !errors.Is(secondErr, store.ErrNotFound) {
		return ack, fmt.Errorf("load force close order: %w", secondErr)
	}

	status, forceReason, err := e.forceClosePosition(ctx, intent)
	if err != nil {
		return ack, err
	}
	if status != "" {
		ack.Status = status
		ack.Reason = forceReason
		return ack, nil
	}

	ack.Status = protocol.CancelCanceled
	if ack.CanceledOrders > 0 {
		ack.Reason = "open order canceled"
	} else {
		ack.Reason = "no open order or position to force close"
	}
	return ack, nil
}

// forceClosePosition sells the token's whole remaining available position at
// 0.01 FAK as an internal child of the intent. It returns an empty status when
// there is nothing to force close and a populated status for outcomes that are
// already terminal from the strategy's point of view (such as a competing
// active sell reservation). A returned error means the exit could not be
// placed at all.
func (e *Executor) forceClosePosition(ctx context.Context, intent store.OrderIntentRecord) (protocol.CancelAckStatus, string, error) {
	position, found, err := e.positionFor(ctx, intent.ConditionID, intent.TokenID)
	if err != nil {
		return "", "", err
	}
	if !found || !decimal.Positive(position.AvailableSize) {
		return "", "", nil
	}

	reservationID := reservationID(intent.IntentID, forceCloseChildSequence)
	now := e.now().UTC()
	if err := e.store.Reserve(ctx, forceCloseReservation(intent, position.AvailableSize, reservationID, now)); err != nil {
		if errors.Is(err, store.ErrActiveSellReservation) {
			return protocol.CancelActiveSellReservation, "another sell order is already active for this token", nil
		}
		if errors.Is(err, store.ErrConflict) {
			return protocol.CancelNoPosition, "no available position to force close", nil
		}
		if err == store.ErrDuplicate {
			// A reservation already exists for this intent's force-close child
			// without a corresponding signed order. Do not place a duplicate.
			return protocol.CancelCompleted, "force close already reserved for intent", nil
		}
		return "", "", fmt.Errorf("reserve force close exposure: %w", err)
	}
	releaseOnFailure := true
	defer func() {
		if releaseOnFailure {
			_ = e.store.Release(context.Background(), reservationID, "force close preparation failed")
		}
	}()

	signed, userOrder, err := e.signForceCloseOrder(ctx, intent, position.AvailableSize)
	if err != nil {
		return "", "", err
	}
	payload, err := json.Marshal(signed)
	if err != nil {
		return "", "", fmt.Errorf("marshal force close order: %w", err)
	}
	hash := sha256.Sum256(payload)
	if err := e.store.PersistSignedOrder(ctx, store.SignedOrderRecord{
		IntentID: intent.IntentID, ChildSequence: forceCloseChildSequence, SignedPayload: payload, SignedOrderHash: fmt.Sprintf("%x", hash[:]),
		Salt: strconv.FormatInt(signed.Salt, 10), ExchangeOrderID: signed.OrderID, RequestedShares: position.AvailableSize, Price: forceClosePrice,
		OrderType: userOrder.OrderType, PostOnly: false, State: statemachine.StateSigned, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return "", "", fmt.Errorf("persist force close order: %w", err)
	}
	releaseOnFailure = false

	// Submit without an intent acknowledgement: this is an internal child, not a
	// new strategy intent. A rejected or unknown submission is terminal here and
	// is reported as a failure on execution.cancel.ack; no retry is scheduled.
	if err := e.submitOrder(ctx, executionIntent(intent), signed, forceCloseChildSequence, 1, userOrder.OrderType, false, false); err != nil {
		return "", "", err
	}
	return protocol.CancelCompleted, "open orders canceled and position force close submitted at 0.01 FAK", nil
}

func (e *Executor) positionFor(ctx context.Context, conditionID, tokenID string) (store.PositionRecord, bool, error) {
	positions, err := e.store.PositionFeatures(ctx)
	if err != nil {
		return store.PositionRecord{}, false, fmt.Errorf("load positions for force close: %w", err)
	}
	for _, candidate := range positions {
		if candidate.ConditionID == conditionID && candidate.TokenID == tokenID {
			return candidate, true, nil
		}
	}
	return store.PositionRecord{}, false, nil
}

func (e *Executor) signForceCloseOrder(ctx context.Context, intent store.OrderIntentRecord, shares string) (clobclient.SignedOrderV2, clobclient.UserOrder, error) {
	userOrder, err := forceCloseUserOrder(intent, shares)
	if err != nil {
		return clobclient.SignedOrderV2{}, clobclient.UserOrder{}, err
	}
	signed, err := e.clob.CreateOrder(ctx, userOrder)
	if err != nil {
		return clobclient.SignedOrderV2{}, clobclient.UserOrder{}, fmt.Errorf("sign force close order: %w", err)
	}
	return signed, userOrder, nil
}

func forceCloseReservation(intent store.OrderIntentRecord, shares, reservationID string, now time.Time) store.ReservationRecord {
	notional := "0"
	if product, ok := decimal.MulString(shares, forceClosePrice); ok {
		notional = product
	}
	return store.ReservationRecord{
		ReservationID: reservationID, IntentID: intent.IntentID, ChildSequence: forceCloseChildSequence,
		MarketID: intent.MarketID, ConditionID: intent.ConditionID, TokenID: intent.TokenID, Outcome: intent.Outcome,
		Side: protocol.SideSell, Shares: shares, Notional: notional, State: "active",
		Reason: "cancel command force close", CreatedAt: now, UpdatedAt: now,
	}
}

func forceCloseUserOrder(intent store.OrderIntentRecord, shares string) (clobclient.UserOrder, error) {
	sharesValue, err := strconv.ParseFloat(strings.TrimSpace(shares), 64)
	if err != nil || sharesValue <= 0 {
		return clobclient.UserOrder{}, fmt.Errorf("invalid available shares %q for force close", shares)
	}
	price, err := decimal.Price(forceClosePrice)
	if err != nil {
		return clobclient.UserOrder{}, err
	}
	return clobclient.UserOrder{TokenID: intent.TokenID, Side: clobclient.SideSell, Shares: sharesValue, Price: price, PostOnly: false, OrderType: clobclient.OrderTypeFAK}, nil
}
