// Package executor persists and submits one child order for each execution intent.
package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
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
	publish protocol.ExecutionEventPublisher
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
			return e.resumeIntent(ctx, protocol.ExecutionIntent{
				IntentID: intent.IntentID, IdempotencyKey: intent.IdempotencyKey, Strategy: intent.Strategy,
				Kind: protocol.IntentKind(intent.Kind), MarketID: intent.MarketID, EventSlug: intent.EventSlug,
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

func (e *Executor) SetEventPublisher(publisher protocol.ExecutionEventPublisher) {
	e.publish = publisher
}

func (e *Executor) publishTransition(order store.SignedOrderRecord, reason string) error {
	return protocol.PublishExecutionOrderEvent(e.publish, order.ExchangeOrderID, string(order.State), order.IntentID, order.MatchedShares, reason, e.now())
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
