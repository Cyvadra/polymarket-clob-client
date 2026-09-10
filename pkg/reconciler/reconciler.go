// Package reconciler repairs unresolved execution state from CLOB REST facts.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/accountfeed"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

const (
	defaultInterval          = 30 * time.Second
	defaultMissingOrderGrace = 2 * time.Minute
)

type CLOB interface {
	Order(context.Context, string) (*clobclient.Order, error)
	AllTrades(context.Context) ([]clobclient.Trade, error)
}

type Reconciler struct {
	store             store.ReconcileStore
	clob              CLOB
	fills             *accountfeed.FillConsumer
	apiKey            string
	now               func() time.Time
	interval          time.Duration
	missingOrderGrace time.Duration
	replayedFills     map[string]struct{}
	onError           func(error)
	publish           protocol.ExecutionEventPublisher
}

func New(repository store.ReconcileStore, clob CLOB, fills *accountfeed.FillConsumer, apiKey string, now func() time.Time, interval time.Duration) (*Reconciler, error) {
	if repository == nil || clob == nil {
		return nil, fmt.Errorf("repository and CLOB client are required")
	}
	if now == nil {
		now = time.Now
	}
	if interval <= 0 {
		interval = defaultInterval
	}
	return &Reconciler{store: repository, clob: clob, fills: fills, apiKey: apiKey, now: now, interval: interval, missingOrderGrace: defaultMissingOrderGrace, replayedFills: map[string]struct{}{}}, nil
}

func (r *Reconciler) SetMissingOrderGrace(grace time.Duration) {
	if grace > 0 {
		r.missingOrderGrace = grace
	}
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

func (r *Reconciler) SetEventPublisher(publisher protocol.ExecutionEventPublisher) {
	r.publish = publisher
}

func (r *Reconciler) Close(context.Context) error { return nil }

func (r *Reconciler) Reconcile(ctx context.Context) error {
	// A fill the store cannot apply must not stall order reconciliation. Order
	// state comes from REST order lookups, not from fills, and an order left
	// unresolved keeps its reservation and never reports a terminal result.
	reconcileErr := r.replayTrades(ctx)
	fillsCurrent := reconcileErr == nil
	orders, err := r.store.OpenOrders(ctx)
	if err != nil {
		return errors.Join(reconcileErr, fmt.Errorf("load unresolved orders: %w", err))
	}
	for _, order := range orders {
		if err := r.reconcileOrder(ctx, order, fillsCurrent); err != nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("reconcile %s/%d: %w", order.IntentID, order.ChildSequence, err))
		}
	}
	return reconcileErr
}

func (r *Reconciler) replayTrades(ctx context.Context) error {
	if r.fills == nil {
		return nil
	}
	trades, err := r.clob.AllTrades(ctx)
	if err != nil {
		return fmt.Errorf("load account trades for reconciliation: %w", err)
	}
	for _, trade := range trades {
		for _, fill := range accountfeed.OwnedFillsFromTrade(trade, r.apiKey, r.now().UTC()) {
			// Applying a fill is idempotent in the store, but the round trip is
			// not free and the trade history only grows. Each fill therefore
			// reaches the store once per process, with a full replay on start.
			if _, seen := r.replayedFills[fill.FillID]; seen {
				continue
			}
			if _, err := r.fills.Consume(ctx, fill); err != nil {
				return fmt.Errorf("replay account trade %s: %w", trade.ID, err)
			}
			if isSettled(fill.TradeStatus) {
				r.replayedFills[fill.FillID] = struct{}{}
			}
		}
	}
	return nil
}

// isSettled reports whether a fill can no longer change, so replaying it again
// could not teach the store anything new.
func isSettled(tradeStatus string) bool {
	switch strings.ToUpper(strings.TrimSpace(tradeStatus)) {
	case "CONFIRMED", "FAILED":
		return true
	default:
		return false
	}
}

// reconcileOrder repairs one unresolved order from its REST observation.
// fillsCurrent reports whether this pass's trade replay succeeded, so the
// store's fills can be trusted as the full record of what the order matched.
func (r *Reconciler) reconcileOrder(ctx context.Context, order store.SignedOrderRecord, fillsCurrent bool) error {
	if order.ExchangeOrderID == "" {
		if order.State == statemachine.StateSigned {
			return nil
		}
		if order.State != statemachine.StateSubmitUnknown {
			return nil
		}
		return r.apply(ctx, order, statemachine.EventReconcileInconclusive, order.MatchedShares, "missing exchange order ID during reconciliation")
	}

	remote, err := r.clob.Order(ctx, order.ExchangeOrderID)
	if err != nil {
		if isMissingOrder(err) && isUnresolvedSubmission(order.State) {
			if r.missingOrderExpired(order) {
				return r.apply(ctx, order, statemachine.EventFailedObserved, order.MatchedShares, "REST order lookup still returned 404 after reconciliation grace period")
			}
			if order.State == statemachine.StateSubmitUnknown || order.State == statemachine.StateSubmitting {
				return r.apply(ctx, order, statemachine.EventReconcileInconclusive, order.MatchedShares, "REST order lookup returned 404 during unresolved submission")
			}
			return nil
		}
		if isMissingOrder(err) && isCancelling(order.State) {
			if !r.missingOrderExpired(order) {
				return nil
			}
			if !fillsCurrent {
				return fmt.Errorf("lookup order %s: %w; not resolved from fills because trade replay failed", order.ExchangeOrderID, err)
			}
			return r.resolveMissingCancel(ctx, order)
		}
		return fmt.Errorf("lookup order %s: %w", order.ExchangeOrderID, err)
	}
	if remote == nil {
		return fmt.Errorf("lookup order %s: empty response", order.ExchangeOrderID)
	}
	return r.applyRemoteOrder(ctx, order, *remote, "REST order observation "+remote.Status)
}

func isMissingOrder(err error) bool {
	var apiErr *clobclient.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func isUnresolvedSubmission(state statemachine.State) bool {
	return state == statemachine.StateSubmitUnknown || state == statemachine.StateUnknownReconcile || state == statemachine.StateSubmitting
}

func isCancelling(state statemachine.State) bool {
	return state == statemachine.StateCancelRequested || state == statemachine.StateCancelPending
}

// resolveMissingCancel settles a cancel whose order the exchange no longer
// returns. The CLOB stops returning some finished orders (a filled order on a
// closed market answers with an empty body), so once the grace period has
// passed the order is gone and its recorded fills are the only account of how
// it ended. Trades are replayed before orders, so those fills are current.
func (r *Reconciler) resolveMissingCancel(ctx context.Context, order store.SignedOrderRecord) error {
	filled, err := r.store.FilledShares(ctx, order.ExchangeOrderID)
	if err != nil {
		return fmt.Errorf("load fills for missing order %s: %w", order.ExchangeOrderID, err)
	}
	// RequestedShares is the planned size, but the signed order was floored to
	// the exchange's share precision, and that is all its fills can reach.
	requested := order.RequestedShares
	intent, err := r.store.Intent(ctx, order.IntentID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("load intent for missing order %s: %w", order.ExchangeOrderID, err)
	}
	if err == nil {
		if floored, ok := decimal.FloorTo(requested, clobclient.SharePrecisionDigits(clobclient.Side(intent.Side), clobclient.OrderType(order.OrderType))); ok {
			requested = floored
		}
	}
	// An order that is gone cannot rest, so it resolves like an immediate
	// order: filled if its fills reach the requested size, otherwise canceled
	// with whatever matched.
	event, _ := statemachine.EventForOrderObservation("MATCHED", filled, requested, true)
	return r.apply(ctx, order, event, filled, "REST order lookup returned 404 after cancel; resolved from recorded fills")
}

func (r *Reconciler) missingOrderExpired(order store.SignedOrderRecord) bool {
	anchor := order.UpdatedAt
	if anchor.IsZero() {
		anchor = order.CreatedAt
	}
	return !anchor.IsZero() && !anchor.Add(r.missingOrderGrace).After(r.now().UTC())
}

func (r *Reconciler) applyRemoteOrder(ctx context.Context, order store.SignedOrderRecord, remote clobclient.Order, reason string) error {
	event, ok := statemachine.EventForOrderObservation(remote.Status, remote.SizeMatched, remote.OriginalSize, statemachine.Immediate(string(order.OrderType)))
	if !ok {
		return fmt.Errorf("order %s has unsupported status %q", remote.ID, remote.Status)
	}
	return r.apply(ctx, order, event, remote.SizeMatched, reason)
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
	if err := protocol.PublishExecutionOrderEvent(r.publish, updated.ExchangeOrderID, string(updated.State), updated.IntentID, updated.MatchedShares, reason, r.now()); err != nil {
		return fmt.Errorf("publish reconciliation event: %w", err)
	}
	// Terminal open results carry identity from the parent intent; skip if the
	// intent is gone (nothing meaningful to report).
	intent, err := r.store.Intent(ctx, updated.IntentID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load reconciliation intent: %w", err)
	}
	if err := accountfeed.PublishTerminalResult(r.publish, intent, updated, reason, r.now()); err != nil {
		return fmt.Errorf("publish reconciliation open result: %w", err)
	}
	return nil
}
