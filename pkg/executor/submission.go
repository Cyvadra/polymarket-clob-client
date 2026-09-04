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
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor/tactics"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type plannedChild struct {
	Sequence    int
	Shares      string
	Price       string
	PostOnly    bool
	TimeInForce protocol.TimeInForce
}

type executionRejection struct {
	code   string
	reason string
	cause  error
}

func (e executionRejection) Error() string { return e.reason }
func (e executionRejection) Unwrap() error { return e.cause }

func (e *Executor) Execute(ctx context.Context, intent protocol.ExecutionIntent) error {
	if err := validateIntentAt(intent, e.now().UTC()); err != nil {
		if strings.TrimSpace(intent.IntentID) != "" {
			e.publishAck(intent.IntentID, protocol.IntentRejected, validationReasonCode(err), publicReason(err), "", "")
		}
		return err
	}
	err := e.store.WithIntentLock(ctx, intent.IntentID, func(ctx context.Context) error {
		inserted, err := e.store.InsertIntent(ctx, intentRecord(intent, e.now().UTC()))
		if err != nil {
			if errors.Is(err, store.ErrIdempotencyConflict) {
				return executionRejection{code: "DUPLICATE_INTENT", reason: "idempotency key already belongs to another intent", cause: err}
			}
			return fmt.Errorf("persist intent: %w", err)
		}
		if !inserted {
			existing, err := e.store.Intent(ctx, intent.IntentID)
			if err != nil {
				return fmt.Errorf("load duplicate intent: %w", err)
			}
			if !sameIntent(intent, existing) {
				return executionRejection{code: "DUPLICATE_INTENT", reason: "intent ID already belongs to a different intent", cause: store.ErrIdempotencyConflict}
			}
			return e.resumeIntent(ctx, executionIntent(existing))
		}
		return e.prepareAndSubmit(ctx, intent)
	})
	if err != nil {
		status, code := protocol.IntentFailed, "EXECUTION_FAILED"
		reason := publicReason(err)
		var rejection executionRejection
		if errors.As(err, &rejection) {
			status, code = protocol.IntentRejected, rejection.code
			reason = rejection.reason
		} else if isRejectedSubmission(err) {
			status, code = protocol.IntentRejected, "ORDER_REJECTED"
		}
		e.publishAck(intent.IntentID, status, code, reason, "", "")
	}
	return err
}

func isRejectedSubmission(err error) bool {
	var rejected *clobclient.OrderRejectedError
	return errors.As(err, &rejected)
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
		e.publishAckForOrder(order, "duplicate intent")
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
	return e.submitSigned(ctx, intent, signed, order.ChildSequence, order.Revision, order.OrderType, order.PostOnly)
}

func (e *Executor) prepareAndSubmit(ctx context.Context, intent protocol.ExecutionIntent) error {
	child, err := e.planInitialChild(intent)
	if err != nil {
		return err
	}
	reservationID := reservationID(intent.IntentID, child.Sequence)
	if err := e.store.Reserve(ctx, reservationRecord(intent, child, reservationID, e.now().UTC())); err != nil {
		if err == store.ErrDuplicate {
			reservation, loadErr := e.store.Reservation(ctx, reservationID)
			if loadErr != nil {
				return fmt.Errorf("load duplicate reservation: %w", loadErr)
			}
			if reservation.State != "active" || reservation.IntentID != intent.IntentID {
				return fmt.Errorf("duplicate reservation is not an active reservation for this intent")
			}
		} else if errors.Is(err, store.ErrConflict) {
			return executionRejection{code: "NO_POSITION", reason: "no available position for close intent", cause: err}
		} else if errors.Is(err, store.ErrActiveSellReservation) {
			return executionRejection{code: "ACTIVE_SELL_RESERVATION", reason: "active sell reservation already exists", cause: err}
		} else if errors.Is(err, store.ErrIdempotencyConflict) {
			return executionRejection{code: "DUPLICATE_INTENT", reason: "idempotency key already belongs to another intent", cause: err}
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
	userOrder, err := userOrder(intent, child)
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
		IntentID: intent.IntentID, ChildSequence: child.Sequence, SignedPayload: payload, SignedOrderHash: fmt.Sprintf("%x", hash[:]),
		Salt: strconv.FormatInt(signed.Salt, 10), ExchangeOrderID: signed.OrderID, RequestedShares: child.Shares, Price: child.Price,
		OrderType: child.TimeInForce, PostOnly: child.PostOnly, State: statemachine.StateSigned, Revision: 1,
		CreatedAt: e.now().UTC(), UpdatedAt: e.now().UTC(),
	}); err != nil {
		return fmt.Errorf("persist signed order: %w", err)
	}
	releaseOnFailure = false
	return e.submitSigned(ctx, intent, signed, child.Sequence, 1, child.TimeInForce, child.PostOnly)
}

func (e *Executor) planInitialChild(intent protocol.ExecutionIntent) (plannedChild, error) {
	var quote marketquotes.Snapshot
	hasQuote := false
	if e.quotes != nil {
		quote, hasQuote = e.quotes.Get(intent.ConditionID)
	}
	decision := tactics.Plan(tactics.Request{Intent: intent, Quote: quote, HasQuote: hasQuote, Now: e.now().UTC()})
	if decision.Action != tactics.ActionSubmitChild {
		return plannedChild{}, fmt.Errorf("execution plan did not produce a child order: %s", decision.Reason)
	}
	return plannedChild{Sequence: decision.NextSequence, Shares: decision.Shares, Price: decision.Price, PostOnly: decision.PostOnly, TimeInForce: decision.TimeInForce}, nil
}

func (e *Executor) submitSigned(ctx context.Context, intent protocol.ExecutionIntent, signed clobclient.SignedOrderV2, childSequence int, revision int64, orderType protocol.TimeInForce, postOnly bool) error {
	order := store.SignedOrderRecord{IntentID: intent.IntentID, ChildSequence: childSequence, State: statemachine.StateSigned, Revision: revision, MatchedShares: "0"}
	updated, err := e.store.TransitionOrder(ctx, order, statemachine.EventSubmitStarted, "0", "", "submit requested")
	if err != nil {
		return fmt.Errorf("mark submitting: %w", err)
	}
	e.publishTransition(updated, "submit requested")
	response, submitErr := e.clob.SubmitSignedOrder(ctx, signed, orderType, postOnly)
	if submitErr != nil {
		if submissionRejected(submitErr) {
			return e.markSubmitRejected(ctx, updated, submitErr)
		}
		return e.markSubmitUnknown(ctx, updated, submitErr.Error())
	}
	if response == nil || response.OrderID == "" {
		return e.markSubmitUnknown(ctx, updated, "successful submission missing exchange order ID")
	}
	if updated.ExchangeOrderID != "" && response.OrderID != updated.ExchangeOrderID {
		return e.markSubmitUnknown(ctx, updated, "submission returned unexpected exchange order ID")
	}
	live, err := e.store.TransitionOrder(ctx, updated, statemachine.EventSubmitAcknowledged, "0", response.OrderID, "submit acknowledged")
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return e.acceptConcurrentSubmitObservation(ctx, intent, childSequence, response.OrderID)
		}
		return fmt.Errorf("mark order live: %w", err)
	}
	e.publishTransition(live, "submit acknowledged")
	e.publishAck(intent.IntentID, protocol.IntentAccepted, "", "submit acknowledged", "0", "")
	return nil
}

func (e *Executor) acceptConcurrentSubmitObservation(ctx context.Context, intent protocol.ExecutionIntent, childSequence int, exchangeOrderID string) error {
	current, err := e.store.OrderByIntent(ctx, intent.IntentID, childSequence)
	if err != nil {
		return fmt.Errorf("load concurrently observed order: %w", err)
	}
	if current.ExchangeOrderID != exchangeOrderID {
		return fmt.Errorf("mark order live: %w", store.ErrConflict)
	}
	switch current.State {
	case statemachine.StateLive, statemachine.StatePartiallyFilled:
		e.publishAck(intent.IntentID, protocol.IntentAccepted, "", "submit acknowledged after concurrent observation", current.MatchedShares, "")
		return nil
	case statemachine.StateFilled, statemachine.StateCanceled, statemachine.StateRejected, statemachine.StateExpired, statemachine.StateFailed:
		e.publishAckForOrder(current, "submit acknowledged after concurrent terminal observation")
		return nil
	default:
		return fmt.Errorf("mark order live: %w", store.ErrConflict)
	}
}

func submissionRejected(err error) bool {
	var rejected *clobclient.OrderRejectedError
	return errors.As(err, &rejected)
}

func (e *Executor) markSubmitUnknown(ctx context.Context, order store.SignedOrderRecord, reason string) error {
	unknown, err := e.store.TransitionOrder(ctx, order, statemachine.EventSubmitTimedOut, order.MatchedShares, order.ExchangeOrderID, reason)
	if err != nil {
		return fmt.Errorf("mark submit unknown: %w", err)
	}
	e.publishTransition(unknown, reason)
	return fmt.Errorf("order submission outcome is unknown: %s", reason)
}

func (e *Executor) markSubmitRejected(ctx context.Context, order store.SignedOrderRecord, submitErr error) error {
	reason := publicReason(submitErr)
	rejected, err := e.store.TransitionOrder(ctx, order, statemachine.EventRejectedObserved, order.MatchedShares, order.ExchangeOrderID, reason)
	if err != nil {
		return fmt.Errorf("mark submit rejected: %w", err)
	}
	e.publishTransition(rejected, reason)
	return executionRejection{code: "ORDER_REJECTED", reason: reason, cause: submitErr}
}

func publicReason(err error) string {
	var rejected *clobclient.OrderRejectedError
	if errors.As(err, &rejected) {
		return rejected.Message
	}
	return "execution failed"
}

func (e *Executor) publishAckForOrder(order store.SignedOrderRecord, reason string) {
	ack, ok := store.TerminalAckForOrder(order, reason, e.now())
	if !ok {
		e.publishAck(order.IntentID, protocol.IntentAccepted, "", reason, order.MatchedShares, "")
		return
	}
	e.publishAck(ack.IntentID, ack.Status, ack.ReasonCode, ack.Reason, ack.FilledShares, ack.AveragePrice)
}
