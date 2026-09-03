// Package reconciler repairs unresolved execution state from CLOB REST facts.
package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/pkg/contracts"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

const defaultInterval = 30 * time.Second

type CLOB interface {
	Order(context.Context, string) (*clobclient.Order, error)
	AllOpenOrders(context.Context) ([]clobclient.Order, error)
	AllTrades(context.Context) ([]clobclient.Trade, error)
}

type Repository interface {
	store.OrderRepository
}

type Reconciler struct {
	store    Repository
	clob     CLOB
	now      func() time.Time
	interval time.Duration
	onError  func(error)
	publish  contracts.ExecutionEventPublisher
}

func New(repository Repository, clob CLOB, now func() time.Time, interval time.Duration) (*Reconciler, error) {
	if repository == nil || clob == nil {
		return nil, fmt.Errorf("repository and CLOB client are required")
	}
	if now == nil {
		now = time.Now
	}
	if interval <= 0 {
		interval = defaultInterval
	}
	return &Reconciler{store: repository, clob: clob, now: now, interval: interval}, nil
}

func (r *Reconciler) Init(ctx context.Context) error {
	if err := r.Reconcile(ctx); err != nil && r.onError != nil {
		r.onError(err)
	}
	return nil
}

func (r *Reconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.Reconcile(ctx); err != nil {
				if r.onError != nil {
					r.onError(err)
				}
			}
		}
	}
}

func (r *Reconciler) SetErrorHandler(handler func(error)) {
	r.onError = handler
}

func (r *Reconciler) SetEventPublisher(publisher contracts.ExecutionEventPublisher) {
	r.publish = publisher
}

func (r *Reconciler) Close(context.Context) error { return nil }

func (r *Reconciler) Reconcile(ctx context.Context) error {
	orders, err := r.store.OpenOrders(ctx)
	if err != nil {
		return fmt.Errorf("load unresolved orders: %w", err)
	}
	var reconcileErr error
	for _, order := range orders {
		if err := r.reconcileOrder(ctx, order); err != nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("reconcile %s/%d: %w", order.IntentID, order.ChildSequence, err))
		}
	}
	return reconcileErr
}

func (r *Reconciler) reconcileOrder(ctx context.Context, order store.SignedOrderRecord) error {
	if order.ExchangeOrderID == "" {
		if order.State != statemachine.StateSubmitUnknown {
			return nil
		}
		if matched, ok, err := r.matchOpenOrder(ctx, order); err != nil {
			return err
		} else if ok {
			event, eventOK := statemachine.EventForOrderStatus(matched.Status)
			if !eventOK {
				return fmt.Errorf("matched order %s has unsupported status %q", matched.ID, matched.Status)
			}
			return r.apply(ctx, withExchangeOrderID(order, matched.ID), event, matched.SizeMatched, "REST open-order recovery "+matched.Status)
		}
		if matched, ok, err := r.matchTrade(ctx, order); err != nil {
			return err
		} else if ok {
			return r.apply(ctx, order, statemachine.EventFillObserved, matched.Size, "REST trade recovery "+matched.ID)
		}
		return r.apply(ctx, order, statemachine.EventReconcileInconclusive, order.MatchedShares, "missing exchange order ID during reconciliation")
	}

	remote, err := r.clob.Order(ctx, order.ExchangeOrderID)
	if err != nil {
		return fmt.Errorf("lookup order %s: %w", order.ExchangeOrderID, err)
	}
	if remote == nil {
		return fmt.Errorf("lookup order %s: empty response", order.ExchangeOrderID)
	}
	event, ok := statemachine.EventForOrderStatus(remote.Status)
	if !ok {
		return fmt.Errorf("order %s has unsupported status %q", order.ExchangeOrderID, remote.Status)
	}
	return r.apply(ctx, order, event, remote.SizeMatched, "REST order observation "+remote.Status)
}

func (r *Reconciler) matchOpenOrder(ctx context.Context, local store.SignedOrderRecord) (clobclient.Order, bool, error) {
	var signed clobclient.SignedOrderV2
	if err := json.Unmarshal(local.SignedPayload, &signed); err != nil {
		return clobclient.Order{}, false, nil
	}
	orders, err := r.clob.AllOpenOrders(ctx)
	if err != nil {
		return clobclient.Order{}, false, fmt.Errorf("load CLOB open orders: %w", err)
	}
	var matched clobclient.Order
	count := 0
	for _, remote := range orders {
		if sameOrder(local, signed, remote) {
			matched = remote
			count++
		}
	}
	return matched, count == 1, nil
}

func (r *Reconciler) matchTrade(ctx context.Context, local store.SignedOrderRecord) (clobclient.Trade, bool, error) {
	var signed clobclient.SignedOrderV2
	if err := json.Unmarshal(local.SignedPayload, &signed); err != nil {
		return clobclient.Trade{}, false, nil
	}
	trades, err := r.clob.AllTrades(ctx)
	if err != nil {
		return clobclient.Trade{}, false, fmt.Errorf("load CLOB trades: %w", err)
	}
	var matched clobclient.Trade
	count := 0
	for _, trade := range trades {
		if sameTrade(local, signed, trade) {
			matched = trade
			count++
		}
	}
	return matched, count == 1, nil
}

func sameOrder(local store.SignedOrderRecord, signed clobclient.SignedOrderV2, remote clobclient.Order) bool {
	if remote.ID == "" || remote.AssetID != signed.TokenID || string(remote.Side) != signed.Side {
		return false
	}
	if remote.CreatedAt > 0 && !local.CreatedAt.IsZero() {
		createdAt := time.Unix(remote.CreatedAt, 0)
		if local.CreatedAt.Sub(createdAt) > 5*time.Minute || createdAt.Sub(local.CreatedAt) > 5*time.Minute {
			return false
		}
	}
	return sameDecimal(remote.Price, local.Price) && sameDecimal(remote.OriginalSize, local.RequestedShares)
}

func sameTrade(local store.SignedOrderRecord, signed clobclient.SignedOrderV2, trade clobclient.Trade) bool {
	if trade.ID == "" || trade.AssetID != signed.TokenID || string(trade.Side) != signed.Side {
		return false
	}
	if !sameDecimal(trade.Price, local.Price) || !sameDecimal(trade.Size, local.RequestedShares) {
		return false
	}
	if trade.Timestamp != "" && !local.CreatedAt.IsZero() {
		if timestamp, err := strconv.ParseInt(trade.Timestamp, 10, 64); err == nil && timestamp > 0 {
			tradeAt := time.Unix(timestamp, 0)
			if local.CreatedAt.Sub(tradeAt) > 5*time.Minute || tradeAt.Sub(local.CreatedAt) > 5*time.Minute {
				return false
			}
		}
	}
	return true
}

func sameDecimal(left, right string) bool {
	leftFloat, leftErr := strconv.ParseFloat(left, 64)
	rightFloat, rightErr := strconv.ParseFloat(right, 64)
	if leftErr != nil || rightErr != nil {
		return left == right
	}
	const epsilon = 1e-9
	return leftFloat-rightFloat < epsilon && rightFloat-leftFloat < epsilon
}

func withExchangeOrderID(order store.SignedOrderRecord, exchangeOrderID string) store.SignedOrderRecord {
	order.ExchangeOrderID = exchangeOrderID
	return order
}

func (r *Reconciler) apply(ctx context.Context, order store.SignedOrderRecord, event statemachine.Event, matchedShares, reason string) error {
	if matchedShares == "" {
		matchedShares = order.MatchedShares
	}
	updated, err := r.store.TransitionOrder(ctx, order, event, matchedShares, order.ExchangeOrderID, reason)
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("persist reconciliation observation for %s/%d: %w", order.IntentID, order.ChildSequence, err)
	}
	if err := contracts.PublishExecutionOrderEvent(r.publish, updated.ExchangeOrderID, string(updated.State), updated.IntentID, updated.MatchedShares, reason, r.now()); err != nil {
		return fmt.Errorf("publish reconciliation event: %w", err)
	}
	return nil
}
