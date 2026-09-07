// Package executor persists and submits one child order for each execution intent.
package executor

import (
	"context"
	"errors"
	"fmt"
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
}

type Executor struct {
	store   repository
	clob    CLOB
	quotes  QuoteProvider
	now     func() time.Time
	onError func(error)
	publish protocol.ExecutionEventPublisher
}

func New(repository repository, clob CLOB, now func() time.Time) (*Executor, error) {
	if repository == nil || clob == nil {
		return nil, fmt.Errorf("repository and CLOB client are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Executor{store: repository, clob: clob, now: now}, nil
}

func (e *Executor) SetQuoteProvider(provider QuoteProvider) {
	e.quotes = provider
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
			return e.submitSignedOrder(ctx, mapping.ExecutionIntent(intent), order)
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

func (e *Executor) publishTransition(order store.SignedOrderRecord, reason string) {
	if err := protocol.PublishExecutionOrderEvent(e.publish, order.ExchangeOrderID, string(order.State), order.IntentID, order.MatchedShares, reason, e.now()); err != nil && e.onError != nil {
		e.onError(fmt.Errorf("publish order transition: %w", err))
	}
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
		if err := e.cancelOpenOrder(ctx, intent, order); err != nil {
			cancelErr = errors.Join(cancelErr, fmt.Errorf("cancel order %s: %w", order.ExchangeOrderID, err))
		}
	}
	return cancelErr
}

// cancelOpenOrder cancels a single still-open exchange order, walking the
// cancel state chain and tolerating state conflicts when another path already
// observed the cancellation. Orders without an exchange order ID or in an
// uncancellable state are left untouched.
func (e *Executor) cancelOpenOrder(ctx context.Context, intent store.OrderIntentRecord, order store.SignedOrderRecord) error {
	if order.ExchangeOrderID == "" {
		return nil
	}
	switch order.State {
	case statemachine.StateLive, statemachine.StatePartiallyFilled, statemachine.StateCancelRequested:
	default:
		return nil
	}
	if order.State != statemachine.StateCancelRequested {
		updated, err := e.store.TransitionOrder(ctx, order, statemachine.EventCancelRequested, order.MatchedShares, order.ExchangeOrderID, "cancellation requested")
		if err != nil {
			if err != store.ErrConflict {
				return fmt.Errorf("mark order %s cancellation requested: %w", order.ExchangeOrderID, err)
			}
			return nil
		}
		order = updated
	}
	cancelCtx, cancel := context.WithTimeout(ctx, cancelTimeout(intent))
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

func deadlinePassed(intent store.OrderIntentRecord, now time.Time) bool {
	if !intent.ExpiresAt.IsZero() && !intent.ExpiresAt.After(now) {
		return true
	}
	policy := mapping.ExecutionIntent(intent).Policy
	return policy.CompleteWithinMillis > 0 && !intent.CreatedAt.IsZero() && !intent.CreatedAt.Add(time.Duration(policy.CompleteWithinMillis)*time.Millisecond).After(now)
}

func cancelTimeout(intent store.OrderIntentRecord) time.Duration {
	policy := mapping.ExecutionIntent(intent).Policy
	if policy.CancelTimeoutMillis > 0 {
		return time.Duration(policy.CancelTimeoutMillis) * time.Millisecond
	}
	return 5 * time.Second
}
