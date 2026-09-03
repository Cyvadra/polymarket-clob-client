// Package executor persists and submits one child order for each execution intent.
package executor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
	"github.com/Cyvadra/polymarket-clob-client/pkg/contracts"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type CLOB interface {
	CreateOrder(context.Context, clobclient.UserOrder) (clobclient.SignedOrderV2, error)
	SubmitSignedOrder(context.Context, clobclient.SignedOrderV2, clobclient.OrderType, bool) (*clobclient.OrderResponse, error)
	CancelOrder(context.Context, string) error
}

type Repository interface {
	store.IntentRepository
	store.IntentLockRepository
	store.OrderRepository
	store.ReservationRepository
}

type Executor struct {
	store   Repository
	clob    CLOB
	now     func() time.Time
	onError func(error)
	publish contracts.ExecutionEventPublisher
}

func New(repository Repository, clob CLOB, now func() time.Time) (*Executor, error) {
	if repository == nil || clob == nil {
		return nil, fmt.Errorf("repository and CLOB client are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Executor{store: repository, clob: clob, now: now}, nil
}

func (e *Executor) Init(context.Context) error { return nil }
func (e *Executor) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := e.resumeSigned(ctx); err != nil && e.onError != nil {
				e.onError(err)
			}
			if err := e.cancelExpired(ctx); err != nil && e.onError != nil {
				e.onError(err)
			}
		}
	}
}

func (e *Executor) resumeSigned(ctx context.Context) error {
	orders, err := e.store.OpenOrders(ctx)
	if err != nil {
		return fmt.Errorf("load signed orders for recovery: %w", err)
	}
	var resumeErr error
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
			return e.resumeIntent(ctx, contracts.ExecutionIntent{
				IntentID: intent.IntentID, IdempotencyKey: intent.IdempotencyKey, Strategy: intent.Strategy,
				Kind: contracts.IntentKind(intent.Kind), MarketID: intent.MarketID, EventSlug: intent.EventSlug,
				ConditionID: intent.ConditionID, TokenID: intent.TokenID, Outcome: intent.Outcome, Side: intent.Side,
				TargetShares: intent.TargetShares, LimitPrice: intent.LimitPrice, TimeInForce: intent.TimeInForce,
				PostOnly: intent.PostOnly, FeatureSeq: intent.FeatureSeq, FeatureCompletedAt: intent.FeatureCompletedAt,
				ExpiresAt: intent.ExpiresAt, Policy: intent.Policy,
			})
		}); err != nil {
			resumeErr = errors.Join(resumeErr, fmt.Errorf("resume signed order %s/%d: %w", order.IntentID, order.ChildSequence, err))
		}
	}
	return resumeErr
}
func (e *Executor) Close(context.Context) error { return nil }

func (e *Executor) SetErrorHandler(handler func(error)) {
	e.onError = handler
}

func (e *Executor) SetEventPublisher(publisher contracts.ExecutionEventPublisher) {
	e.publish = publisher
}

func (e *Executor) publishTransition(order store.SignedOrderRecord, reason string) error {
	return contracts.PublishExecutionOrderEvent(e.publish, order.ExchangeOrderID, string(order.State), order.IntentID, order.MatchedShares, reason, e.now())
}

func (e *Executor) cancelExpired(ctx context.Context) error {
	orders, err := e.store.OpenOrders(ctx)
	if err != nil {
		return fmt.Errorf("load open orders for deadline cancellation: %w", err)
	}
	var cancelErr error
	for _, order := range orders {
		if order.ExchangeOrderID == "" || (order.State != statemachine.StateLive && order.State != statemachine.StatePartiallyFilled && order.State != statemachine.StateCancelRequested) {
			continue
		}
		intent, err := e.store.Intent(ctx, order.IntentID)
		if err != nil {
			cancelErr = errors.Join(cancelErr, fmt.Errorf("load intent %s: %w", order.IntentID, err))
			continue
		}
		if !deadlinePassed(intent, e.now().UTC()) {
			continue
		}
		if order.State != statemachine.StateCancelRequested {
			updated, err := e.store.TransitionOrder(ctx, order, statemachine.EventCancelRequested, order.MatchedShares, order.ExchangeOrderID, "execution deadline elapsed")
			if err != nil {
				if err != store.ErrConflict {
					cancelErr = errors.Join(cancelErr, fmt.Errorf("mark order %s cancellation requested: %w", order.ExchangeOrderID, err))
				}
				continue
			}
			order = updated
		}
		cancelCtx, cancel := context.WithTimeout(ctx, cancelTimeout(intent))
		err = e.clob.CancelOrder(cancelCtx, order.ExchangeOrderID)
		cancel()
		if err != nil {
			cancelErr = errors.Join(cancelErr, fmt.Errorf("cancel order %s: %w", order.ExchangeOrderID, err))
			continue
		}
		if _, err := e.store.TransitionOrder(ctx, order, statemachine.EventCancelAccepted, order.MatchedShares, order.ExchangeOrderID, "cancellation accepted"); err != nil && err != store.ErrConflict {
			cancelErr = errors.Join(cancelErr, fmt.Errorf("mark order %s cancellation pending: %w", order.ExchangeOrderID, err))
		}
	}
	return cancelErr
}

func deadlinePassed(intent store.OrderIntentRecord, now time.Time) bool {
	if !intent.ExpiresAt.IsZero() && !intent.ExpiresAt.After(now) {
		return true
	}
	return intent.Policy.CompleteWithinMillis > 0 && !intent.CreatedAt.IsZero() && !intent.CreatedAt.Add(time.Duration(intent.Policy.CompleteWithinMillis)*time.Millisecond).After(now)
}

func cancelTimeout(intent store.OrderIntentRecord) time.Duration {
	if intent.Policy.CancelTimeoutMillis > 0 {
		return time.Duration(intent.Policy.CancelTimeoutMillis) * time.Millisecond
	}
	return 5 * time.Second
}

func (e *Executor) Execute(ctx context.Context, intent contracts.ExecutionIntent) error {
	if err := validateIntentAt(intent, e.now().UTC()); err != nil {
		e.publishAck(intent.IntentID, contracts.IntentRejected, "INVALID_INTENT", err.Error(), "", "")
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
		status, code := contracts.IntentFailed, "EXECUTION_FAILED"
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			status, code = contracts.IntentRejected, "NO_POSITION"
		} else if isRejectedSubmission(err) {
			status, code = contracts.IntentRejected, "ORDER_REJECTED"
		}
		e.publishAck(intent.IntentID, status, code, err.Error(), "", "")
	}
	return err
}

func isRejectedSubmission(err error) bool {
	return strings.Contains(err.Error(), "order submission rejected:")
}

func (e *Executor) publishAck(intentID string, status contracts.IntentAckStatus, code, reason, filledShares, averagePrice string) {
	if err := contracts.PublishExecutionIntentAck(e.publish, contracts.ExecutionIntentAck{
		IntentID: intentID, Status: status, ReasonCode: code, Reason: reason,
		FilledShares: filledShares, AveragePrice: averagePrice, OccurredAt: e.now(),
	}); err != nil && e.onError != nil {
		e.onError(fmt.Errorf("publish intent acknowledgement: %w", err))
	}
}

func (e *Executor) resumeIntent(ctx context.Context, intent contracts.ExecutionIntent) error {
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

func (e *Executor) prepareAndSubmit(ctx context.Context, intent contracts.ExecutionIntent) error {
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

func (e *Executor) submitSigned(ctx context.Context, intent contracts.ExecutionIntent, signed clobclient.SignedOrderV2, revision int64) error {
	order := store.SignedOrderRecord{IntentID: intent.IntentID, ChildSequence: 1, State: statemachine.StateSigned, Revision: revision, MatchedShares: "0"}
	updated, err := e.store.TransitionOrder(ctx, order, statemachine.EventSubmitStarted, "0", "", "submit requested")
	if err != nil {
		return fmt.Errorf("mark submitting: %w", err)
	}
	if err := e.publishTransition(updated, "submit requested"); err != nil {
		return fmt.Errorf("publish submitting event: %w", err)
	}
	response, submitErr := e.clob.SubmitSignedOrder(ctx, signed, clobOrderType(intent.TimeInForce), intent.PostOnly)
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
	e.publishAck(intent.IntentID, contracts.IntentAccepted, "", "submit acknowledged", "0", "")
	return nil
}

func submissionRejected(err error) bool {
	var apiErr *transport.HTTPError
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

func validateIntent(intent contracts.ExecutionIntent) error {
	return validateIntentAt(intent, time.Now().UTC())
}

func validateIntentAt(intent contracts.ExecutionIntent, now time.Time) error {
	if intent.IntentID == "" || intent.IdempotencyKey == "" || intent.Strategy == "" || intent.ConditionID == "" || intent.TokenID == "" || intent.Outcome == "" {
		return fmt.Errorf("intent ID, idempotency key, strategy, condition ID, token ID, and outcome are required")
	}
	if intent.Side != contracts.SideBuy && intent.Side != contracts.SideSell {
		return fmt.Errorf("invalid side %q", intent.Side)
	}
	shares, err := strconv.ParseFloat(intent.TargetShares, 64)
	if err != nil || shares <= 0 {
		return fmt.Errorf("invalid target shares: %w", err)
	}
	price, err := strconv.ParseFloat(intent.LimitPrice, 64)
	if err != nil || price <= 0 || price >= 1 {
		return fmt.Errorf("invalid limit price %q", intent.LimitPrice)
	}
	if intent.TimeInForce != contracts.TimeInForceGTC && intent.TimeInForce != contracts.TimeInForceFOK && intent.TimeInForce != contracts.TimeInForceFAK && intent.TimeInForce != contracts.TimeInForceGTD {
		return fmt.Errorf("invalid time in force %q", intent.TimeInForce)
	}
	if !intent.ExpiresAt.IsZero() && !intent.ExpiresAt.After(now) {
		return fmt.Errorf("execution intent expired at %s", intent.ExpiresAt.UTC().Format(time.RFC3339Nano))
	}
	if intent.ExpiresAt.IsZero() && intent.Policy.CompleteWithinMillis == 0 {
		return fmt.Errorf("execution intent requires expires_at or complete_within_ms")
	}
	if intent.Policy.CompleteWithinMillis < 0 || intent.Policy.CancelTimeoutMillis < 0 || intent.Policy.MaxFeatureAgeMillis < 0 || intent.Policy.MaxReprices < 0 || intent.Policy.RepriceDelayMillis < 0 {
		return fmt.Errorf("execution policy durations must not be negative")
	}
	if intent.Kind != contracts.IntentOpen && intent.Kind != contracts.IntentClose {
		return fmt.Errorf("invalid intent kind %q", intent.Kind)
	}
	if intent.Kind == contracts.IntentClose && intent.Side != contracts.SideSell {
		return fmt.Errorf("close execution intent must sell")
	}
	if intent.Policy.MaxFeatureAgeMillis > 0 && (intent.FeatureCompletedAt.IsZero() || now.Sub(intent.FeatureCompletedAt) > time.Duration(intent.Policy.MaxFeatureAgeMillis)*time.Millisecond) {
		return fmt.Errorf("execution intent feature is stale")
	}
	if intent.Policy.Style != "" && intent.Policy.Style != contracts.ExecutionStyleLimit && intent.Policy.Style != contracts.ExecutionStyleMakerPostOnly && intent.Policy.Style != contracts.ExecutionStyleTakerRepricing {
		return fmt.Errorf("invalid execution style %q", intent.Policy.Style)
	}
	return nil
}

func intentRecord(intent contracts.ExecutionIntent, now time.Time) store.OrderIntentRecord {
	return store.OrderIntentRecord{
		IntentID: intent.IntentID, IdempotencyKey: intent.IdempotencyKey, Strategy: intent.Strategy, MarketID: intent.MarketID,
		Kind:      intent.Kind,
		EventSlug: intent.EventSlug, ConditionID: intent.ConditionID, TokenID: intent.TokenID, Outcome: intent.Outcome,
		Side: intent.Side, TargetShares: intent.TargetShares, LimitPrice: intent.LimitPrice,
		TimeInForce: intent.TimeInForce, PostOnly: intent.PostOnly, FeatureSeq: intent.FeatureSeq,
		FeatureCompletedAt: intent.FeatureCompletedAt, ExpiresAt: intent.ExpiresAt, Status: statemachine.StateIntentReceived,
		Policy: intent.Policy, CreatedAt: now, UpdatedAt: now,
	}
}

func reservationRecord(intent contracts.ExecutionIntent, reservationID string, now time.Time) store.ReservationRecord {
	notional := "0"
	if shares, sharesOK := newRat(intent.TargetShares); sharesOK {
		if price, priceOK := newRat(intent.LimitPrice); priceOK {
			notional = new(big.Rat).Mul(shares, price).FloatString(18)
		}
	}
	return store.ReservationRecord{
		ReservationID: reservationID,
		IntentID:      intent.IntentID,
		ChildSequence: 1,
		MarketID:      intent.MarketID,
		ConditionID:   intent.ConditionID,
		TokenID:       intent.TokenID,
		Outcome:       intent.Outcome,
		Side:          intent.Side,
		Shares:        intent.TargetShares,
		Notional:      notional,
		State:         "active",
		Reason:        "execution intent accepted",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func reservationID(intentID string, childSequence int) string {
	return fmt.Sprintf("%s:%d", intentID, childSequence)
}

func newRat(value string) (*big.Rat, bool) {
	return new(big.Rat).SetString(value)
}

func userOrder(intent contracts.ExecutionIntent) (clobclient.UserOrder, error) {
	shares, err := strconv.ParseFloat(intent.TargetShares, 64)
	if err != nil || shares <= 0 {
		return clobclient.UserOrder{}, fmt.Errorf("invalid target shares %q", intent.TargetShares)
	}
	price, err := strconv.ParseFloat(intent.LimitPrice, 64)
	if err != nil || price <= 0 || price >= 1 {
		return clobclient.UserOrder{}, fmt.Errorf("invalid limit price %q", intent.LimitPrice)
	}
	side := clobclient.Side(intent.Side)
	return clobclient.UserOrder{TokenID: intent.TokenID, Side: side, Shares: shares, Price: price, PostOnly: intent.PostOnly, OrderType: clobOrderType(intent.TimeInForce)}, nil
}

func clobOrderType(value contracts.TimeInForce) clobclient.OrderType {
	return clobclient.OrderType(value)
}
