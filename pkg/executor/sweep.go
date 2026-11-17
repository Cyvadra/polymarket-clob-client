package executor

import (
	"context"
	"errors"
	"fmt"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// OpenOrderLister lists the account's open orders on the exchange.
type OpenOrderLister interface {
	AllOpenOrders(context.Context) ([]clobclient.Order, error)
}

// SweepUnknownOrders cancels every open exchange order that has no stored
// row. An order's row commits alongside its POST, so a crash between the two
// leaves a live order no observer can see: recovery works from rows. Run it
// at startup, before intents are taken, when no order of this process can be
// mid-commit. It cancels any order the account holds without a row, including
// one placed by hand or by another tool on the same account.
func (e *Executor) SweepUnknownOrders(ctx context.Context, lister OpenOrderLister) error {
	orders, err := lister.AllOpenOrders(ctx)
	if err != nil {
		return fmt.Errorf("list open orders for the unknown-order sweep: %w", err)
	}
	var unknown []string
	for _, order := range orders {
		if order.ID == "" {
			continue
		}
		_, err := e.store.OrderByExchangeID(ctx, order.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			unknown = append(unknown, order.ID)
		case err != nil:
			return fmt.Errorf("look up open order %s: %w", order.ID, err)
		}
	}
	var errs error
	for start := 0; start < len(unknown); start += clobclient.MaxBatchOrders {
		batch := unknown[start:min(start+clobclient.MaxBatchOrders, len(unknown))]
		result, err := e.clob.CancelOrdersResult(ctx, batch)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("%w: cancel unknown open orders %v: %w", ErrOrphanAlert, batch, err))
			continue
		}
		for _, id := range batch {
			if result.Has(id) {
				e.reportError(fmt.Errorf("%w: open order %s had no stored row; it was canceled", ErrOrphanAlert, id))
			} else {
				errs = errors.Join(errs, fmt.Errorf("%w: open order %s has no stored row and was not canceled: %s", ErrOrphanAlert, id, result.NotCanceled[id]))
			}
		}
	}
	return errs
}
