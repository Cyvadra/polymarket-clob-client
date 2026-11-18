package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeInventory struct {
	complementErr error
	level         store.InventoryLevel
	position      *store.PositionRecord
	asked         string
	sellToken     string
	deadline      time.Time
}

func (f *fakeInventory) Complement(_ context.Context, _, tokenID string) (string, string, error) {
	if f.complementErr != nil {
		return "", "", f.complementErr
	}
	if tokenID == "up" {
		return "down", "Down", nil
	}
	return "up", "Up", nil
}

func (f *fakeInventory) AwaitInventory(_ context.Context, _, _, sellToken, shares string, deadline time.Time) (store.InventoryLevel, store.PositionRecord, bool, error) {
	f.asked, f.sellToken, f.deadline = shares, sellToken, deadline
	if f.position == nil {
		return f.level, store.PositionRecord{}, false, nil
	}
	return f.level, *f.position, true, nil
}

func preserveBuy(now time.Time) protocol.ExecutionIntent {
	return protocol.ExecutionIntent{
		SchemaVersion: protocol.SchemaVersionV1, IntentID: "i", RequestID: "r", UniqueTag: "lane", Strategy: "s", Kind: protocol.IntentOpen,
		ConditionID: "c", TokenID: "up", Outcome: "Up", Side: protocol.SideBuy, TargetUSD: "50", LimitPrice: "0.1",
		TimeInForce: protocol.TimeInForceGTC, ExpiresAt: now.Add(time.Minute), SignalPrice: "0.12", MaxSignalDrift: "0.02",
		Policy: protocol.ExecutionPolicy{Style: protocol.ExecutionStyleSplitPreserve},
	}
}

func TestPreserveIntentSellsTheComplementCappedByInventory(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	inventory := &fakeInventory{level: store.InventoryLevel{Remaining: "100"}}
	e := &Executor{now: func() time.Time { return now }, inventory: inventory}
	sell, _, err := e.preserveIntent(context.Background(), preserveBuy(now))
	if err != nil {
		t.Fatal(err)
	}
	if decimal.Compare(inventory.asked, "500") != 0 {
		t.Fatalf("asked the inventory for %s shares, want 500 ($50 at 0.1)", inventory.asked)
	}
	if sell.UniqueTag != "lane|inv" || sell.TokenID != "down" || sell.Outcome != "Down" || sell.Side != protocol.SideSell {
		t.Fatalf("sell is not the complement on the inventory lane: %+v", sell)
	}
	if decimal.Compare(sell.LimitPrice, "0.9") != 0 || decimal.Compare(sell.SignalPrice, "0.88") != 0 {
		t.Fatalf("complement prices: limit %s signal %s", sell.LimitPrice, sell.SignalPrice)
	}
	leg := sell.Policy.Preserve
	if leg == nil || leg.UniqueTag != "lane" || leg.TokenID != "up" || decimal.Compare(leg.RequestedShares, "500") != 0 || decimal.Compare(leg.Shares, "100") != 0 {
		t.Fatalf("preserve leg: %+v", leg)
	}
	if err := validateIntentAt(sell, now); err != nil {
		t.Fatalf("the sell it places is invalid: %v", err)
	}
}

func TestPreserveIntentRefusesWithoutInventory(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	e := &Executor{now: func() time.Time { return now }, inventory: &fakeInventory{level: store.InventoryLevel{Remaining: "0"}}}
	_, _, err := e.preserveIntent(context.Background(), preserveBuy(now))
	if _, code, _ := reasonFor(err); code != protocol.ReasonInsufficientInventory {
		t.Fatalf("got %v (%s), want %s", err, code, protocol.ReasonInsufficientInventory)
	}
}

func TestPreserveIntentRefusesWithoutInventoryBeforeAComplementError(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	complementErr := errors.New("market unknown")
	inventory := &fakeInventory{complementErr: complementErr, level: store.InventoryLevel{Remaining: "0"}}
	e := &Executor{now: func() time.Time { return now }, inventory: inventory}
	_, _, err := e.preserveIntent(context.Background(), preserveBuy(now))
	if _, code, _ := reasonFor(err); code != protocol.ReasonInsufficientInventory {
		t.Fatalf("got %v (%s), want %s", err, code, protocol.ReasonInsufficientInventory)
	}
	inventory.level.Remaining = "100"
	if _, _, err = e.preserveIntent(context.Background(), preserveBuy(now)); !errors.Is(err, complementErr) {
		t.Fatalf("with inventory got %v, want the complement error", err)
	}
}

func TestPreserveIntentRefusesWhenDisabledOrForged(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	e := &Executor{now: func() time.Time { return now }}
	if _, _, err := e.preserveIntent(context.Background(), preserveBuy(now)); err == nil {
		t.Fatal("accepted a preserve with no inventory configured")
	}
	e.inventory = &fakeInventory{level: store.InventoryLevel{Remaining: "100"}}
	forged := preserveBuy(now)
	forged.Policy.Preserve = &protocol.PreserveLeg{Shares: "1"}
	if _, _, err := e.preserveIntent(context.Background(), forged); err == nil {
		t.Fatal("accepted a request that carried policy.preserve")
	}
}

func TestInventoryLaneTakesOnlyPreserveSells(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	open := preserveBuy(now)
	open.Policy = protocol.ExecutionPolicy{}
	open.UniqueTag = "lane|inv"
	if err := validateIntentAt(open, now); err == nil {
		t.Fatal("accepted a plain open on an inventory lane")
	}
	closeReq := protocol.ExecutionCloseRequest{SchemaVersion: protocol.SchemaVersionV1, UniqueTag: "lane|inv", Strategy: "s",
		ConditionID: "c", AssetID: "down", Outcome: "Down", Mode: protocol.ExecutionCloseModeForce}
	if err := validateCloseRequest(closeReq); err == nil {
		t.Fatal("accepted a close on an inventory lane")
	}
}

func TestPlanningUsesThePositionAPreserveRead(t *testing.T) {
	e := &Executor{} // no store: a read would panic
	sell := protocol.ExecutionIntent{ConditionID: "c", TokenID: "down", UniqueTag: "lane|inv"}
	held := store.PositionRecord{ConditionID: "c", TokenID: "down", UniqueTag: "lane|inv", ActualShares: "7"}
	ctx := context.WithValue(context.Background(), lanePositionKey{}, held)
	got, found, err := e.carriedPosition(ctx, sell)
	if err != nil || !found || got.ActualShares != "7" {
		t.Fatalf("carried position not used: %+v %v %v", got, found, err)
	}
}

func TestPreserveIntentCarriesTheSoldTokensPosition(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	held := store.PositionRecord{ConditionID: "c", TokenID: "up", ActualShares: "40"}
	inventory := &fakeInventory{level: store.InventoryLevel{Remaining: "100"}, position: &held}
	e := &Executor{now: func() time.Time { return now }, inventory: inventory}
	sell, position, err := e.preserveIntent(context.Background(), preserveBuy(now))
	if err != nil {
		t.Fatal(err)
	}
	if inventory.sellToken != sell.TokenID {
		t.Fatalf("read the position of %q, sells %q", inventory.sellToken, sell.TokenID)
	}
	if position == nil || position.ActualShares != "40" {
		t.Fatalf("position not carried: %+v", position)
	}
	inventory.position = nil
	if _, position, err = e.preserveIntent(context.Background(), preserveBuy(now)); err != nil || position != nil {
		t.Fatalf("got %+v, %v; want no position", position, err)
	}
}
