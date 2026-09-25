// Package accountfeed consumes normalized account events from the shared event bus.
package accountfeed

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// unknownOrderReportLimit caps how many distinct out-of-band orders are
// reported individually. A wallet that traded outside executiond before it was
// deployed can carry hundreds of them, and one line each drowns the log
// without telling an operator anything the first few lines did not.
const unknownOrderReportLimit = 10

// FeeSchedules looks up a market's fee schedule; *clobclient.Client
// implements it.
type FeeSchedules interface {
	FeeSchedule(ctx context.Context, conditionID string) (clobclient.FeeSchedule, error)
}

type FillConsumer struct {
	store   store.AccountFillStore
	now     func() time.Time
	onError func(error)
	onFill  func()
	fees    FeeSchedules

	mu              sync.Mutex
	unknownOrders   map[string]struct{}
	unknownReported int
}

func NewFillConsumer(repository store.AccountFillStore, now func() time.Time) (*FillConsumer, error) {
	if repository == nil {
		return nil, fmt.Errorf("fill store is required")
	}
	if now == nil {
		now = time.Now
	}
	return &FillConsumer{store: repository, now: now, unknownOrders: map[string]struct{}{}}, nil
}

// SetErrorHandler receives non-fatal observations the consumer cannot recover
// from, such as a fill whose exchange order is unknown to the store.
func (c *FillConsumer) SetErrorHandler(handler func(error)) {
	c.onError = handler
}

// SetFillHook is called after each fill is newly stored, so caches of the
// wallet balance a fill moves can be dropped.
func (c *FillConsumer) SetFillHook(hook func()) {
	c.onFill = hook
}

// SetFeeSchedules makes the consumer record the fee each fill paid. The
// exchange reports fee_rate_bps 0 and no fee on crypto trades that are in fact
// charged, so without a schedule every fill is stored fee-free.
func (c *FillConsumer) SetFeeSchedules(fees FeeSchedules) {
	c.fees = fees
}

// Consume stores a fill exactly once. The caller may safely retry a delivery
// whose acknowledgement was lost because FillID is the durable idempotency key.
func (c *FillConsumer) Consume(ctx context.Context, fill AccountFill) (bool, error) {
	if err := validateFill(fill); err != nil {
		return false, err
	}
	if fill.ExchangeOrderID == "" {
		return false, fmt.Errorf("exchange order ID is required for an account fill")
	}
	order, err := c.store.OrderByExchangeID(ctx, fill.ExchangeOrderID)
	if err == store.ErrNotFound {
		// A fill for an order this executiond never signed means the wallet was
		// traded outside the runtime, or a fill raced ahead of order
		// persistence. The position accounting cannot safely absorb it, so it
		// is dropped, but never silently: surface it so operators can
		// investigate the divergence.
		c.reportUnknownFill(fill)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lookup fill order: %w", err)
	}
	intent, err := c.store.Intent(ctx, order.IntentID)
	if err != nil {
		return false, fmt.Errorf("lookup fill intent: %w", err)
	}
	receivedAt := fill.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = c.now().UTC()
	}
	if fill.Fee == "" {
		fill.Fee = c.fee(ctx, fill)
	}
	inserted, err := c.store.ApplyFill(ctx, store.FillRecord{
		FillID: fill.FillID, ExchangeOrderID: fill.ExchangeOrderID, IntentID: order.IntentID, UniqueTag: intent.UniqueTag,
		MarketID: fill.MarketID, ConditionID: fill.ConditionID, TokenID: fill.TokenID,
		Outcome: fill.Outcome, Side: store.Side(fill.Side), Shares: fill.Shares, Price: fill.Price,
		Fee: fill.Fee, FeeRateBps: fill.FeeRateBps, TradeStatus: fill.TradeStatus,
		TraderSide: fill.TraderSide, ExchangeTime: fill.ExchangeTime, ReceivedAt: receivedAt,
	})
	if inserted && c.onFill != nil {
		c.onFill()
	}
	return inserted, err
}

// fee derives what fill paid from its market's schedule. A fill is stored
// exactly once, so a failed lookup costs the fee column and nothing else: it is
// reported and the fill is kept, because the position must not wait on it.
func (c *FillConsumer) fee(ctx context.Context, fill AccountFill) string {
	if c.fees == nil {
		return ""
	}
	side := strings.ToUpper(strings.TrimSpace(fill.TraderSide))
	if side != "TAKER" && side != "MAKER" {
		c.report(fmt.Errorf("fill %s: unknown trader side %q, fee not recorded", fill.FillID, fill.TraderSide))
		return ""
	}
	schedule, err := c.fees.FeeSchedule(ctx, fill.ConditionID)
	if err != nil {
		c.report(fmt.Errorf("fill %s: load fee schedule for %s: %w", fill.FillID, fill.ConditionID, err))
		return ""
	}
	fee, err := schedule.Fee(fill.Shares, fill.Price, side == "TAKER")
	if err != nil {
		c.report(fmt.Errorf("fill %s: compute fee: %w", fill.FillID, err))
		return ""
	}
	return fee
}

func (c *FillConsumer) report(err error) {
	if c.onError != nil {
		c.onError(err)
	}
}

// UnknownOrderCount reports how many distinct exchange orders have produced
// dropped fills. It keeps growing after individual reporting is capped, so
// operators can still see the size of the divergence.
func (c *FillConsumer) UnknownOrderCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.unknownOrders)
}

// reportUnknownFill surfaces an out-of-band order once. The same order fills
// many times and the reconciler replays the account trade history on every
// pass, so reporting per fill repeats the same fact indefinitely; reporting
// per order, up to unknownOrderReportLimit, keeps the signal without the
// flood.
func (c *FillConsumer) reportUnknownFill(fill AccountFill) {
	if c.onError == nil {
		return
	}
	c.mu.Lock()
	if _, seen := c.unknownOrders[fill.ExchangeOrderID]; seen {
		c.mu.Unlock()
		return
	}
	c.unknownOrders[fill.ExchangeOrderID] = struct{}{}
	total := len(c.unknownOrders)
	c.unknownReported++
	reported := c.unknownReported
	c.mu.Unlock()

	if reported > unknownOrderReportLimit {
		return
	}
	if reported == unknownOrderReportLimit {
		c.onError(fmt.Errorf("dropping fills for exchange order %s; %d distinct orders not placed by executiond have now been seen, suppressing further per-order reports",
			fill.ExchangeOrderID, total))
		return
	}
	c.onError(fmt.Errorf("dropping fill %s: exchange order %s is not known to executiond (condition=%s token=%s outcome=%s side=%s shares=%s)",
		fill.FillID, fill.ExchangeOrderID, fill.ConditionID, fill.TokenID, fill.Outcome, fill.Side, fill.Shares))
}

func validateFill(fill AccountFill) error {
	if fill.SchemaVersion != "" && fill.SchemaVersion != protocol.SchemaVersionV1 {
		return fmt.Errorf("unsupported fill schema version %q", fill.SchemaVersion)
	}
	if fill.FillID == "" || fill.ConditionID == "" || fill.TokenID == "" || fill.Outcome == "" {
		return fmt.Errorf("fill ID, condition ID, token ID, and outcome are required")
	}
	if fill.Side != protocol.SideBuy && fill.Side != protocol.SideSell {
		return fmt.Errorf("invalid fill side %q", fill.Side)
	}
	if !decimal.Positive(fill.Shares) {
		return fmt.Errorf("invalid fill shares %q", fill.Shares)
	}
	if _, err := decimal.Price(fill.Price); err != nil {
		return fmt.Errorf("invalid fill price %q", fill.Price)
	}
	if fill.Fee != "" && !decimal.NonNegative(fill.Fee) {
		return fmt.Errorf("invalid fill fee %q", fill.Fee)
	}
	if fill.FeeRateBps != "" && !decimal.NonNegative(fill.FeeRateBps) {
		return fmt.Errorf("invalid fill fee rate %q", fill.FeeRateBps)
	}
	switch fill.TradeStatus {
	case "", "MATCHED", "MINED", "CONFIRMED", "FAILED":
	default:
		return fmt.Errorf("invalid trade status %q", fill.TradeStatus)
	}
	return nil
}
