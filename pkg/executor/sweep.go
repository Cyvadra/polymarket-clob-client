package executor

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// OpenOrderLister lists the account's open orders on the exchange and
// resolves an order's market so the sweep can match it by slug.
type OpenOrderLister interface {
	AllOpenOrders(context.Context) ([]clobclient.Order, error)
	Market(ctx context.Context, conditionID string) (*clobclient.Market, error)
}

// UnknownOrderSweep scopes SweepUnknownOrders. With Disabled set, or no
// MarketPrefixes, the sweep does nothing.
type UnknownOrderSweep struct {
	Disabled bool
	// MarketPrefixes are market slug patterns. One without glob characters
	// is a prefix ("btc-updown-5m"); one with them ("*-updown-*") must match
	// the whole slug, as path.Match does.
	MarketPrefixes []string
}

// ValidateMarketPattern reports a malformed glob pattern.
func ValidateMarketPattern(pattern string) error {
	_, err := path.Match(pattern, "")
	return err
}

func (o UnknownOrderSweep) matches(slug string) bool {
	for _, pattern := range o.MarketPrefixes {
		if !strings.ContainsAny(pattern, `*?[\`) {
			if strings.HasPrefix(slug, pattern) {
				return true
			}
			continue
		}
		if ok, _ := path.Match(pattern, slug); ok {
			return true
		}
	}
	return false
}

// SweepUnknownOrders cancels every open exchange order that has no stored
// row and whose market slug matches one of opts.MarketPrefixes. An order's
// row commits alongside its POST, so a crash between the two leaves a live
// order no observer can see: recovery works from rows. Run it at startup,
// before intents are taken, when no order of this process can be mid-commit.
// Orders in other markets, such as ones placed by hand, are left alone; an
// order whose market cannot be resolved is never canceled.
func (e *Executor) SweepUnknownOrders(ctx context.Context, lister OpenOrderLister, opts UnknownOrderSweep) error {
	if opts.Disabled || len(opts.MarketPrefixes) == 0 {
		return nil
	}
	orders, err := lister.AllOpenOrders(ctx)
	if err != nil {
		return fmt.Errorf("list open orders for the unknown-order sweep: %w", err)
	}
	var unknown []string
	var lookupErrs error
	slugs := map[string]string{}
	for _, order := range orders {
		if order.ID == "" {
			continue
		}
		_, err := e.store.OrderByExchangeID(ctx, order.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return fmt.Errorf("look up open order %s: %w", order.ID, err)
		default:
			continue
		}
		slug, ok := slugs[order.Market]
		if !ok {
			market, err := lister.Market(ctx, order.Market)
			if err != nil || market == nil {
				lookupErrs = errors.Join(lookupErrs, fmt.Errorf("resolve market %q of unknown open order %s; left open: %w", order.Market, order.ID, err))
				continue
			}
			slug = market.Slug
			slugs[order.Market] = slug
		}
		if opts.matches(slug) {
			unknown = append(unknown, order.ID)
		}
	}
	errs := lookupErrs
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
