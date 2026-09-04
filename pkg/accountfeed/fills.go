// Package accountfeed consumes normalized account events from the shared event bus.
package accountfeed

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type FillStore interface {
	store.FillRepository
	OrderByExchangeID(context.Context, string) (store.SignedOrderRecord, error)
}

type FillConsumer struct {
	store FillStore
	now   func() time.Time
}

func NewFillConsumer(repository FillStore, now func() time.Time) (*FillConsumer, error) {
	if repository == nil {
		return nil, fmt.Errorf("fill store is required")
	}
	if now == nil {
		now = time.Now
	}
	return &FillConsumer{store: repository, now: now}, nil
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
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lookup filled order: %w", err)
	}
	if order.IntentID == "" {
		return false, fmt.Errorf("filled order %s has no intent ID", fill.ExchangeOrderID)
	}
	fill.IntentID = order.IntentID
	receivedAt := fill.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = c.now().UTC()
	}
	return c.store.ApplyFill(ctx, store.FillRecord{
		FillID: fill.FillID, ExchangeOrderID: fill.ExchangeOrderID, IntentID: fill.IntentID,
		MarketID: fill.MarketID, ConditionID: fill.ConditionID, TokenID: fill.TokenID,
		Outcome: fill.Outcome, Side: fill.Side, Shares: fill.Shares, Price: fill.Price,
		Fee: fill.Fee, FeeRateBps: fill.FeeRateBps, TradeStatus: fill.TradeStatus,
		TraderSide: fill.TraderSide, ExchangeTime: fill.ExchangeTime, ReceivedAt: receivedAt,
	})
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
	if !positiveDecimal(fill.Shares) {
		return fmt.Errorf("invalid fill shares %q", fill.Shares)
	}
	price, ok := new(big.Rat).SetString(fill.Price)
	if !ok || price.Sign() <= 0 || price.Cmp(big.NewRat(1, 1)) >= 0 {
		return fmt.Errorf("invalid fill price %q", fill.Price)
	}
	if fill.Fee != "" && !nonNegativeDecimal(fill.Fee) {
		return fmt.Errorf("invalid fill fee %q", fill.Fee)
	}
	if fill.FeeRateBps != "" && !nonNegativeDecimal(fill.FeeRateBps) {
		return fmt.Errorf("invalid fill fee rate %q", fill.FeeRateBps)
	}
	switch fill.TradeStatus {
	case "", "MATCHED", "MINED", "CONFIRMED", "FAILED":
	default:
		return fmt.Errorf("invalid trade status %q", fill.TradeStatus)
	}
	return nil
}

func positiveDecimal(value string) bool {
	parsed, ok := new(big.Rat).SetString(value)
	return ok && parsed.Sign() > 0
}

func nonNegativeDecimal(value string) bool {
	parsed, ok := new(big.Rat).SetString(value)
	return ok && parsed.Sign() >= 0
}
