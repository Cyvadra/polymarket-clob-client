package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// Inventory is the split inventory a SPLIT_PRESERVE open sells from
// (pkg/inventory).
type Inventory interface {
	// Complement names the other outcome token of a binary condition.
	Complement(ctx context.Context, conditionID, tokenID string) (complementID, outcome string, err error)
	// AwaitInventory reads a lane's inventory, waiting while it is short of
	// shares and a split is still in flight, until deadline, with the
	// inventory lane's position in sellToken from the same read.
	AwaitInventory(ctx context.Context, uniqueTag, conditionID, sellToken, shares string, deadline time.Time) (store.InventoryLevel, store.PositionRecord, bool, error)
}

// SetInventory enables SPLIT_PRESERVE opens.
func (e *Executor) SetInventory(inventory Inventory) { e.inventory = inventory }

// executePreserve works a SPLIT_PRESERVE open: a BUY of shares of one token,
// placed as a SELL of as many of its complement out of the lane's split
// inventory at one minus the limit price. Holding a pair and selling one side
// leaves exactly the other side, at the same cost a BUY would have paid, so
// the strategy sees an ordinary BUY: the store moves the bought shares to the
// strategy lane as the sell fills, and the result is reported as the BUY.
func (e *Executor) executePreserve(ctx context.Context, intent protocol.ExecutionIntent) error {
	sell, position, err := e.preserveIntent(ctx, intent)
	if err != nil {
		e.publishOpenRejection(intent, err)
		return err
	}
	if position != nil {
		ctx = context.WithValue(ctx, lanePositionKey{}, *position)
	}
	if err := e.Execute(ctx, sell); err != nil {
		var unresolved unresolvedSubmission
		if !errors.As(err, &unresolved) {
			e.publishOpenRejection(sell, err)
		}
		return err
	}
	return nil
}

// preserveIntent turns a SPLIT_PRESERVE BUY into the SELL it places, sized by
// the inventory that backs it. It also returns the inventory lane's position
// in the sold token, read with the level, for planning to use in place of a
// read of its own (nil when the lane holds none).
func (e *Executor) preserveIntent(ctx context.Context, intent protocol.ExecutionIntent) (protocol.ExecutionIntent, *store.PositionRecord, error) {
	if e.inventory == nil {
		return intent, nil, reject(protocol.ReasonUnsupportedStyle, "split inventory is not enabled on this executiond")
	}
	if intent.Policy.Preserve != nil {
		return intent, nil, invalid("policy.preserve is set by executiond only")
	}
	if intent.Side != protocol.SideBuy {
		return intent, nil, invalid("%s opens a buy", protocol.ExecutionStyleSplitPreserve)
	}
	if protocol.IsInventoryTag(intent.UniqueTag) {
		return intent, nil, invalid("unique tag %q names an inventory lane", intent.UniqueTag)
	}
	now := e.now().UTC()
	if err := validateIntentAt(intent, now); err != nil {
		return intent, nil, err
	}
	complementPrice, ok := complement(intent.LimitPrice)
	if !ok {
		return intent, nil, invalid("invalid limit price %q", intent.LimitPrice)
	}
	digits := clobclient.SharePrecisionDigits(protocol.SideSell, intent.TimeInForce)
	requested, ok := decimal.DivideAndRoundDown(intent.TargetUSD, intent.LimitPrice, digits)
	if !ok {
		return intent, nil, reject(protocol.ReasonUnplannable, "target usd %s buys less than one share unit at %s", intent.TargetUSD, intent.LimitPrice)
	}
	// The complement comes from memory once the market is known, so it is
	// resolved first and the level read can bring the sold token's position.
	// A lane with no free inventory is still refused as such when the
	// complement cannot be found: its error waits until the level is read.
	complementID, complementOutcome, complementErr := e.inventory.Complement(ctx, intent.ConditionID, intent.TokenID)
	inventoryStart := time.Now()
	level, position, found, err := e.inventory.AwaitInventory(ctx, intent.UniqueTag, intent.ConditionID, complementID, requested, preserveDeadline(intent, now))
	stage(ctx, inventoryStart, stageInventory)
	if err != nil {
		return intent, nil, fmt.Errorf("read split inventory: %w", err)
	}
	shares := requested
	if decimal.Compare(level.Remaining, shares) < 0 {
		shares = level.Remaining
	}
	if shares, ok = decimal.FloorTo(shares, digits); !ok || !decimal.Positive(shares) {
		return intent, nil, reject(protocol.ReasonInsufficientInventory, "lane holds no free split inventory for %s shares", requested)
	}
	if complementErr != nil {
		return intent, nil, fmt.Errorf("find complement token: %w", complementErr)
	}
	sell := intent
	sell.UniqueTag = protocol.InventoryTag(intent.UniqueTag)
	sell.TokenID, sell.Outcome, sell.Side, sell.LimitPrice = complementID, complementOutcome, protocol.SideSell, complementPrice
	// The drift bound reads the complement's mid: the requested token
	// falling under its signal is the complement rising over one minus it,
	// which is how a SELL's bound reads.
	if intent.SignalPrice != "" {
		if sell.SignalPrice, ok = complement(intent.SignalPrice); !ok {
			return intent, nil, invalid("invalid signal price %q", intent.SignalPrice)
		}
	}
	sell.Policy.MidPrice, sell.Policy.InitialPrice, sell.Policy.MaxPrice, sell.Policy.MinPrice = "", "", "", ""
	sell.Policy.Preserve = &protocol.PreserveLeg{
		UniqueTag: intent.UniqueTag, TokenID: intent.TokenID, Outcome: intent.Outcome,
		LimitPrice: intent.LimitPrice, SignalPrice: intent.SignalPrice,
		RequestedShares: requested, Shares: shares,
	}
	if !found {
		return sell, nil, nil
	}
	return sell, &position, nil
}

// preserveDeadline is how long an open may wait for a split in flight: until
// it would expire anyway.
func preserveDeadline(intent protocol.ExecutionIntent, now time.Time) time.Time {
	deadline := intent.ExpiresAt
	if within := intent.Policy.CompleteWithinMillis; within > 0 {
		if byWindow := now.Add(time.Duration(within) * time.Millisecond); deadline.IsZero() || byWindow.Before(deadline) {
			deadline = byWindow
		}
	}
	return deadline
}

// complement is 1 - price, for a price inside (0,1).
func complement(price string) (string, bool) {
	if _, err := decimal.Price(price); err != nil {
		return "", false
	}
	out, ok := decimal.SubString("1", price)
	return out, ok
}

// CancelInventoryOrders withdraws every SPLIT_PRESERVE entry resting against
// a lane's inventory, on either token, before the inventory is merged.
func (e *Executor) CancelInventoryOrders(ctx context.Context, uniqueTag, conditionID string, tokens []string) error {
	var errs error
	for _, token := range tokens {
		errs = errors.Join(errs, e.cancelLaneOpenOrders(ctx, protocol.ExecutionCloseRequest{
			SchemaVersion: protocol.SchemaVersionV1, UniqueTag: protocol.InventoryTag(uniqueTag),
			ConditionID: conditionID, AssetID: token, Mode: protocol.ExecutionCloseModeCancelOpen,
		}))
	}
	return errs
}
