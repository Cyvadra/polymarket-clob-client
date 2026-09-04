package executor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

func (e *Executor) Execute(ctx context.Context, intent protocol.ExecutionIntent) error {
	if err := validateIntentAt(intent, e.now().UTC()); err != nil {
		if strings.TrimSpace(intent.IntentID) != "" {
			e.publishAck(intent.IntentID, protocol.IntentRejected, validationReasonCode(err), err.Error(), "", "")
		}
		return err
	}
	err := e.store.WithIntentLock(ctx, intent.IntentID, func(ctx context.Context) error {
		inserted, err := e.store.InsertIntent(ctx, intentRecord(intent, e.now().UTC()))
		if err != nil {
			return fmt.Errorf("persist intent: %w", err)
		}
		if !inserted {
			return e.resumeIntent(ctx, intent)
		}
		return e.prepareAndSubmit(ctx, intent)
	})
	if err != nil {
		status, code := protocol.IntentFailed, "EXECUTION_FAILED"
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			status, code = protocol.IntentRejected, "NO_POSITION"
		} else if isRejectedSubmission(err) {
			status, code = protocol.IntentRejected, "ORDER_REJECTED"
		}
		e.publishAck(intent.IntentID, status, code, err.Error(), "", "")
	}
	return err
}

func isRejectedSubmission(err error) bool {
	return strings.Contains(err.Error(), "order submission rejected:")
}

func (e *Executor) publishAck(intentID string, status protocol.IntentAckStatus, code, reason, filledShares, averagePrice string) {
	if err := protocol.PublishExecutionIntentAck(e.publish, protocol.ExecutionIntentAck{
		IntentID: intentID, Status: status, ReasonCode: code, Reason: reason,
		FilledShares: filledShares, AveragePrice: averagePrice, OccurredAt: e.now(),
	}); err != nil && e.onError != nil {
		e.onError(fmt.Errorf("publish intent acknowledgement: %w", err))
	}
}

func (e *Executor) resumeIntent(ctx context.Context, intent protocol.ExecutionIntent) error {
	order, err := e.store.OrderByIntent(ctx, intent.IntentID, 1)
	if err != nil {
		if err == store.ErrNotFound {
			return e.prepareAndSubmit(ctx, intent)
		}
		return fmt.Errorf("load existing order: %w", err)
	}
	if order.State != statemachine.StateSigned {
		return nil
	}
	reservation, err := e.store.Reservation(ctx, reservationID(intent.IntentID, order.ChildSequence))
	if err != nil {
		return fmt.Errorf("load signed order reservation: %w", err)
	}
	if reservation.State != "active" {
		return fmt.Errorf("signed order reservation is not active")
	}
	var signed clobclient.SignedOrderV2
	if err := json.Unmarshal(order.SignedPayload, &signed); err != nil {
		return fmt.Errorf("decode persisted signed order: %w", err)
	}
	return e.submitSigned(ctx, intent, signed, order.Revision)
}

func (e *Executor) prepareAndSubmit(ctx context.Context, intent protocol.ExecutionIntent) error {
	reservationID := reservationID(intent.IntentID, 1)
	if err := e.store.Reserve(ctx, reservationRecord(intent, reservationID, e.now().UTC())); err != nil {
		if err == store.ErrDuplicate {
			reservation, loadErr := e.store.Reservation(ctx, reservationID)
			if loadErr != nil {
				return fmt.Errorf("load duplicate reservation: %w", loadErr)
			}
			if reservation.State != "active" || reservation.IntentID != intent.IntentID {
				return fmt.Errorf("duplicate reservation is not an active reservation for this intent")
			}
		} else {
			return fmt.Errorf("reserve order exposure: %w", err)
		}
	}
	releaseOnFailure := true
	defer func() {
		if releaseOnFailure {
			_ = e.store.Release(context.Background(), reservationID, "prepare order failed")
		}
	}()
	userOrder, err := userOrder(intent)
	if err != nil {
		return err
	}
	signed, err := e.clob.CreateOrder(ctx, userOrder)
	if err != nil {
		return fmt.Errorf("sign order: %w", err)
	}
	payload, err := json.Marshal(signed)
	if err != nil {
		return fmt.Errorf("marshal signed order: %w", err)
	}
	hash := sha256.Sum256(payload)
	if err := e.store.PersistSignedOrder(ctx, store.SignedOrderRecord{
		IntentID: intent.IntentID, ChildSequence: 1, SignedPayload: payload, SignedOrderHash: fmt.Sprintf("%x", hash[:]),
		Salt: strconv.FormatInt(signed.Salt, 10), RequestedShares: intent.TargetShares, Price: intent.LimitPrice,
		OrderType: intent.TimeInForce, PostOnly: intent.PostOnly, State: statemachine.StateSigned, Revision: 1,
		CreatedAt: e.now().UTC(), UpdatedAt: e.now().UTC(),
	}); err != nil {
		return fmt.Errorf("persist signed order: %w", err)
	}
	releaseOnFailure = false
	return e.submitSigned(ctx, intent, signed, 1)
}

func (e *Executor) submitSigned(ctx context.Context, intent protocol.ExecutionIntent, signed clobclient.SignedOrderV2, revision int64) error {
	order := store.SignedOrderRecord{IntentID: intent.IntentID, ChildSequence: 1, State: statemachine.StateSigned, Revision: revision, MatchedShares: "0"}
	updated, err := e.store.TransitionOrder(ctx, order, statemachine.EventSubmitStarted, "0", "", "submit requested")
	if err != nil {
		return fmt.Errorf("mark submitting: %w", err)
	}
	if err := e.publishTransition(updated, "submit requested"); err != nil {
		return fmt.Errorf("publish submitting event: %w", err)
	}
	response, submitErr := e.clob.SubmitSignedOrder(ctx, signed, intent.TimeInForce, intent.PostOnly)
	if submitErr != nil {
		if submissionRejected(submitErr) {
			return e.markSubmitRejected(ctx, updated, submitErr.Error())
		}
		return e.markSubmitUnknown(ctx, updated, submitErr.Error())
	}
	if response == nil || response.OrderID == "" {
		return e.markSubmitUnknown(ctx, updated, "successful submission missing exchange order ID")
	}
	live, err := e.store.TransitionOrder(ctx, updated, statemachine.EventSubmitAcknowledged, "0", response.OrderID, "submit acknowledged")
	if err != nil {
		return fmt.Errorf("mark order live: %w", err)
	}
	if err := e.publishTransition(live, "submit acknowledged"); err != nil {
		return fmt.Errorf("publish live event: %w", err)
	}
	e.publishAck(intent.IntentID, protocol.IntentAccepted, "", "submit acknowledged", "0", "")
	return nil
}

func submissionRejected(err error) bool {
	var apiErr *clobclient.APIError
	return errors.As(err, &apiErr) && !apiErr.Retryable()
}

func (e *Executor) markSubmitUnknown(ctx context.Context, order store.SignedOrderRecord, reason string) error {
	unknown, err := e.store.TransitionOrder(ctx, order, statemachine.EventSubmitTimedOut, order.MatchedShares, order.ExchangeOrderID, reason)
	if err != nil {
		return fmt.Errorf("mark submit unknown: %w", err)
	}
	if err := e.publishTransition(unknown, reason); err != nil {
		return fmt.Errorf("publish submit-unknown event: %w", err)
	}
	return fmt.Errorf("order submission outcome is unknown: %s", reason)
}

func (e *Executor) markSubmitRejected(ctx context.Context, order store.SignedOrderRecord, reason string) error {
	rejected, err := e.store.TransitionOrder(ctx, order, statemachine.EventRejectedObserved, order.MatchedShares, order.ExchangeOrderID, reason)
	if err != nil {
		return fmt.Errorf("mark submit rejected: %w", err)
	}
	if err := e.publishTransition(rejected, reason); err != nil {
		return fmt.Errorf("publish submit-rejected event: %w", err)
	}
	return fmt.Errorf("order submission rejected: %s", reason)
}
