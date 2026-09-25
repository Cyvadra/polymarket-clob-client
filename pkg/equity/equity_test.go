package equity

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeBalances struct {
	balance string
	err     error
	calls   int
}

func (f *fakeBalances) BalanceAllowance(_ context.Context, assetType, tokenID string) (*clobclient.BalanceAllowance, error) {
	f.calls++
	if assetType != "COLLATERAL" || tokenID != "" {
		return nil, errors.New("unexpected asset")
	}
	if f.err != nil {
		return nil, f.err
	}
	return &clobclient.BalanceAllowance{Balance: f.balance}, nil
}

type fakePositions []store.PositionRecord

func (f fakePositions) PositionFeatures(context.Context) ([]store.PositionRecord, error) {
	return f, nil
}

type fakeQuotes map[string]marketquotes.Snapshot

func (f fakeQuotes) Get(conditionID string) (marketquotes.Snapshot, bool) {
	snapshot, ok := f[conditionID]
	return snapshot, ok
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestSnapshotMarksPositionsAtBid(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	balances := &fakeBalances{balance: "100500000"}
	positions := fakePositions{
		{ConditionID: "c1", TokenID: "up1", UniqueTag: "a", PositionSize: "10", EntryPrice: "0.40"},
		{ConditionID: "c1", TokenID: "down1", UniqueTag: "b", PositionSize: "4", EntryPrice: "0.55"},
		{ConditionID: "c2", TokenID: "up2", UniqueTag: "c", PositionSize: "5", EntryPrice: "0.30"},
		{ConditionID: "c1", TokenID: "up1", UniqueTag: "d", PositionSize: "0", EntryPrice: "0.40"},
	}
	quotes := fakeQuotes{"c1": {ConditionID: "c1", At: now, Up: marketquotes.Quote{AssetID: "up1", Bid: 0.45}, Down: marketquotes.Quote{AssetID: "down1", Bid: 0}}}
	tracker, err := New(balances, positions, quotes, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := tracker.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 10×0.45 at bid, 4×0 (fresh quote, no bid), 5×0.30 unmarked at entry.
	if !near(snapshot.CashUSD, 100.5) || !near(snapshot.PositionsUSD, 6) || !near(snapshot.EquityUSD, 106.5) {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if snapshot.Positions != 3 || snapshot.UnmarkedPositions != 1 {
		t.Fatalf("counts=%+v", snapshot)
	}
}

func TestStaleQuoteFallsBackToEntryPrice(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	positions := fakePositions{{ConditionID: "c1", TokenID: "up1", PositionSize: "10", EntryPrice: "0.40"}}
	quotes := fakeQuotes{"c1": {ConditionID: "c1", At: now.Add(-time.Minute), Up: marketquotes.Quote{AssetID: "up1", Bid: 0.9}}}
	tracker, _ := New(&fakeBalances{balance: "0"}, positions, quotes, func() time.Time { return now })
	tracker.SetMaxQuoteAge(10 * time.Second)
	snapshot, err := tracker.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !near(snapshot.PositionsUSD, 4) || snapshot.UnmarkedPositions != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestCashIsCachedUntilTTLOrInvalidate(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	balances := &fakeBalances{balance: "1000000"}
	tracker, _ := New(balances, fakePositions{}, fakeQuotes{}, func() time.Time { return now })
	for range 3 {
		if _, _, err := tracker.Cash(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if balances.calls != 1 {
		t.Fatalf("calls=%d, want 1", balances.calls)
	}
	now = now.Add(DefaultCashTTL)
	tracker.Cash(context.Background())
	tracker.Invalidate()
	tracker.Cash(context.Background())
	if balances.calls != 3 {
		t.Fatalf("calls=%d, want 3", balances.calls)
	}
}

func TestSnapshotFailsWithoutCash(t *testing.T) {
	tracker, _ := New(&fakeBalances{err: errors.New("down")}, fakePositions{}, fakeQuotes{}, nil)
	if _, err := tracker.Snapshot(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	tracker, _ = New(&fakeBalances{balance: "-1"}, fakePositions{}, fakeQuotes{}, nil)
	if _, err := tracker.Snapshot(context.Background()); err == nil {
		t.Fatal("expected parse error")
	}
}
