package equity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/settlement"
)

type walletBalances struct {
	cash   string
	tokens map[string]string

	mu    sync.Mutex
	reads map[string]int
}

func (w *walletBalances) TokenBalance(ctx context.Context, tokenID string) (string, error) {
	balance, err := w.BalanceAllowance(ctx, "CONDITIONAL", tokenID)
	if err != nil {
		return "", err
	}
	return balance.Balance, nil
}

func (w *walletBalances) BalanceAllowance(_ context.Context, assetType, tokenID string) (*clobclient.BalanceAllowance, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reads == nil {
		w.reads = map[string]int{}
	}
	w.reads[assetType+"/"+tokenID]++
	switch assetType {
	case "COLLATERAL":
		return &clobclient.BalanceAllowance{Balance: w.cash}, nil
	case "CONDITIONAL":
		return &clobclient.BalanceAllowance{Balance: w.tokens[tokenID]}, nil
	}
	return nil, errors.New("unexpected asset")
}

type fakeMarkets struct {
	markets map[string]clobclient.Market
	err     error

	mu    sync.Mutex
	reads map[string]int
}

func (f *fakeMarkets) Market(_ context.Context, conditionID string) (*clobclient.Market, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reads == nil {
		f.reads = map[string]int{}
	}
	f.reads[conditionID]++
	if f.err != nil {
		return nil, f.err
	}
	m, ok := f.markets[conditionID]
	if !ok {
		return nil, errors.New("no such market")
	}
	return &m, nil
}

func resolved(winner, loser string) clobclient.Market {
	return clobclient.Market{Closed: true, Tokens: []clobclient.Token{{TokenID: winner, Price: 1, Winner: true}, {TokenID: loser}}}
}

func TestSettledPositionsAreValuedAtTheirOutcome(t *testing.T) {
	now := time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)
	balances := &walletBalances{cash: "50000000", tokens: map[string]string{
		"w1": "8000000", // a winner still in the wallet, less than the lane recorded
		"w2": "0",       // a winner already redeemed into cash
	}}
	settled := now.Add(-settlement.SettleGrace)
	positions := fakePositions{
		{ConditionID: "lost", TokenID: "l1", UniqueTag: "a", PositionSize: "30", EntryPrice: "0.90", UpdatedAt: settled},
		{ConditionID: "won", TokenID: "w1", UniqueTag: "b", PositionSize: "10", EntryPrice: "0.85", UpdatedAt: settled},
		{ConditionID: "redeemed", TokenID: "w2", UniqueTag: "c", PositionSize: "20", EntryPrice: "0.88", UpdatedAt: settled},
		{ConditionID: "pending", TokenID: "p1", UniqueTag: "d", PositionSize: "5", EntryPrice: "0.80", UpdatedAt: settled},
		{ConditionID: "live", TokenID: "up", UniqueTag: "e", PositionSize: "4", EntryPrice: "0.70", UpdatedAt: settled},
		// A winner bought a moment ago: its balance reads 0 until the tokens
		// land, so it is taken at what the lane recorded.
		{ConditionID: "redeemed", TokenID: "w2", UniqueTag: "f", PositionSize: "3", EntryPrice: "0.88", UpdatedAt: now.Add(-time.Second)},
	}
	markets := &fakeMarkets{markets: map[string]clobclient.Market{
		"lost":     resolved("l2", "l1"),
		"won":      resolved("w1", "w1x"),
		"redeemed": resolved("w2", "w2x"),
		// Closed, but the resolution has not landed: no winner yet.
		"pending": {Closed: true, Tokens: []clobclient.Token{{TokenID: "p1"}, {TokenID: "p2"}}},
	}}
	quotes := fakeQuotes{"live": {ConditionID: "live", At: now, Up: marketquotes.Quote{AssetID: "up", Bid: 0.75}}}
	tracker, err := New(balances, positions, quotes, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := settlement.NewResolver(markets, balances, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	tracker.SetSettlements(resolver)
	snapshot, err := tracker.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// lost 0 + won min(10, 8)×1 + redeemed 0 + pending 5×0.80 at entry + live 4×0.75 + fresh 3×1.
	if !near(snapshot.PositionsUSD, 8+4+3+3) || !near(snapshot.EquityUSD, 50+18) {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if snapshot.Positions != 6 || snapshot.SettledPositions != 4 || snapshot.UnmarkedPositions != 1 || snapshot.LookupFailures != 0 {
		t.Fatalf("counts=%+v", snapshot)
	}
	if markets.reads["live"] != 0 || balances.reads["CONDITIONAL/l1"] != 0 {
		t.Fatalf("looked up more than it needed: markets=%v balances=%v", markets.reads, balances.reads)
	}

}

// Lanes holding the same winning token share its wallet balance in store
// order rather than each counting the whole of it.
func TestSettledLanesShareAWalletBalance(t *testing.T) {
	now := time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)
	settled := now.Add(-settlement.SettleGrace)
	balances := &walletBalances{cash: "0", tokens: map[string]string{"w": "15000000"}}
	positions := fakePositions{
		{ConditionID: "m", TokenID: "w", UniqueTag: "a", PositionSize: "10", EntryPrice: "0.5", UpdatedAt: settled},
		{ConditionID: "m", TokenID: "w", UniqueTag: "b", PositionSize: "10", EntryPrice: "0.5", UpdatedAt: settled},
		{ConditionID: "m", TokenID: "w", UniqueTag: "c", PositionSize: "10", EntryPrice: "0.5", UpdatedAt: settled},
	}
	markets := &fakeMarkets{markets: map[string]clobclient.Market{"m": resolved("w", "l")}}
	tracker, _ := New(balances, positions, fakeQuotes{}, func() time.Time { return now })
	resolver, _ := settlement.NewResolver(markets, balances, func() time.Time { return now })
	tracker.SetSettlements(resolver)
	snapshot, err := tracker.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !near(snapshot.PositionsUSD, 15) || snapshot.SettledPositions != 3 {
		t.Fatalf("snapshot=%+v, want the 15 shares counted once", snapshot)
	}
}

// One market the CLOB cannot serve must not stop every valuation: the lane is
// valued at its entry price like any unmarked one, and the failure is counted.
func TestSettledLookupFailureFallsBackToEntryPrice(t *testing.T) {
	now := time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)
	settled := now.Add(-settlement.SettleGrace)
	positions := fakePositions{
		{ConditionID: "gone", TokenID: "t", UniqueTag: "a", PositionSize: "10", EntryPrice: "0.9", UpdatedAt: settled},
		{ConditionID: "lost", TokenID: "l1", UniqueTag: "b", PositionSize: "10", EntryPrice: "0.9", UpdatedAt: settled},
	}
	balances := &walletBalances{cash: "0"}
	markets := &fakeMarkets{markets: map[string]clobclient.Market{"lost": resolved("l2", "l1")}}
	tracker, _ := New(balances, positions, fakeQuotes{}, func() time.Time { return now })
	resolver, _ := settlement.NewResolver(markets, balances, func() time.Time { return now })
	tracker.SetSettlements(resolver)
	for range 3 {
		snapshot, err := tracker.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !near(snapshot.PositionsUSD, 9) || snapshot.UnmarkedPositions != 1 || snapshot.LookupFailures != 1 || snapshot.SettledPositions != 1 {
			t.Fatalf("snapshot=%+v", snapshot)
		}
	}
	if markets.reads["gone"] != 1 {
		t.Fatalf("market reads=%v, want the failure held between snapshots", markets.reads)
	}
}
