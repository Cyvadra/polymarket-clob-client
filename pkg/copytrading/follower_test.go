package copytrading

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

type fakeTrader struct {
	mu            sync.Mutex
	tick, minSize float64
	balance       string
	// settled, when set, is the balance reported once this many balance
	// reads have passed, standing in for a fill that settles late.
	settled      string
	settleReads  int
	balanceReads int
	submitted    []clobclient.UserOrder
}

func (f *fakeTrader) TickSize(context.Context, string) (float64, error)     { return f.tick, nil }
func (f *fakeTrader) MinOrderSize(context.Context, string) (float64, error) { return f.minSize, nil }

func (f *fakeTrader) SubmitOrder(_ context.Context, order clobclient.UserOrder) (*clobclient.OrderResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitted = append(f.submitted, order)
	return &clobclient.OrderResponse{Success: true, OrderID: fmt.Sprint(len(f.submitted)), Status: "matched", TakingAmount: fmt.Sprint(order.Shares)}, nil
}

func (f *fakeTrader) BalanceAllowance(context.Context, string, string) (*clobclient.BalanceAllowance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balanceReads++
	if f.settled != "" && f.balanceReads > f.settleReads {
		return &clobclient.BalanceAllowance{Balance: f.settled}, nil
	}
	return &clobclient.BalanceAllowance{Balance: f.balance}, nil
}

var now = time.UnixMilli(1_790_000_000_000)

func newFollower(t *testing.T, trader *fakeTrader, dryRun bool) *Follower {
	t.Helper()
	f, err := New(Config{USD: 10, InitialDiff: 0.02, MaxTradeAge: 5 * time.Second, DryRun: dryRun, SettleWait: time.Second, SettlePoll: time.Millisecond, Now: func() time.Time { return now }}, trader)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func activity(side string, price float64) Activity {
	return Activity{AssetID: "tok", Address: "0xabc", Outcome: "Up", Side: side, Price: price, Size: 25, Timestamp: now.UnixMilli() - 400}
}

func TestBuyUsesPriceplusInitialDiffAndFixedUSD(t *testing.T) {
	trader := &fakeTrader{tick: 0.01, minSize: 5}
	newFollower(t, trader, false).Handle(context.Background(), activity("BUY", 0.61))
	if len(trader.submitted) != 1 {
		t.Fatalf("expected one order, got %+v", trader.submitted)
	}
	order := trader.submitted[0]
	if math.Abs(order.Price-0.63) > 1e-9 || order.OrderType != clobclient.OrderTypeFAK || order.Side != clobclient.SideBuy {
		t.Fatalf("unexpected order %+v", order)
	}
	if order.Shares != 15.873 {
		t.Fatalf("shares = %v, want 15.873 ($10 / 0.63)", order.Shares)
	}
}

func TestBuyPriceIsCappedBelowOne(t *testing.T) {
	trader := &fakeTrader{tick: 0.01}
	newFollower(t, trader, false).Handle(context.Background(), activity("BUY", 0.985))
	if len(trader.submitted) != 1 || trader.submitted[0].Price != maxBuyPrice {
		t.Fatalf("expected a 0.99 buy, got %+v", trader.submitted)
	}
}

func TestSellUsesFloorPriceAndIsCappedByHoldings(t *testing.T) {
	trader := &fakeTrader{tick: 0.01, balance: "100000000"} // 100 shares
	newFollower(t, trader, false).Handle(context.Background(), activity("SELL", 0.5))
	if len(trader.submitted) != 1 || trader.submitted[0].Shares != 20 || trader.submitted[0].Price != 0.01 {
		t.Fatalf("expected a 20-share sell at 0.01, got %+v", trader.submitted)
	}

	trader = &fakeTrader{tick: 0.01, balance: "4000000"} // 4 shares
	newFollower(t, trader, false).Handle(context.Background(), activity("SELL", 0.5))
	if len(trader.submitted) != 1 || trader.submitted[0].Shares != 4 {
		t.Fatalf("expected the whole 4-share position sold, got %+v", trader.submitted)
	}
}

func TestSkips(t *testing.T) {
	stale := activity("BUY", 0.5)
	stale.Timestamp = now.Add(-10 * time.Second).UnixMilli()
	noAsset := activity("BUY", 0.5)
	noAsset.AssetID = ""
	cases := map[string]struct {
		trader   *fakeTrader
		activity Activity
	}{
		"stale":          {&fakeTrader{tick: 0.01}, stale},
		"unknown asset":  {&fakeTrader{tick: 0.01}, noAsset},
		"no position":    {&fakeTrader{tick: 0.01, balance: "0"}, activity("SELL", 0.5)},
		"below min size": {&fakeTrader{tick: 0.01, minSize: 50}, activity("BUY", 0.5)},
	}
	for name, tc := range cases {
		newFollower(t, tc.trader, false).Handle(context.Background(), tc.activity)
		if len(tc.trader.submitted) != 0 {
			t.Errorf("%s: expected no order, got %+v", name, tc.trader.submitted)
		}
	}
}

func TestDryRunSubmitsNothing(t *testing.T) {
	trader := &fakeTrader{tick: 0.01}
	newFollower(t, trader, true).Handle(context.Background(), activity("BUY", 0.5))
	if len(trader.submitted) != 0 {
		t.Fatalf("dry run submitted %+v", trader.submitted)
	}
}

func TestSellWaitsForRecentBuyToSettle(t *testing.T) {
	// The buy fills 15.873 shares, but the balance still reads 0 for the
	// first three reads after it.
	trader := &fakeTrader{tick: 0.01, balance: "0", settled: "15873000", settleReads: 3}
	f := newFollower(t, trader, false)
	f.Enqueue(activity("BUY", 0.61))
	f.Enqueue(activity("SELL", 0.62))
	f.Close()
	if len(trader.submitted) != 2 || trader.submitted[0].Side != clobclient.SideBuy || trader.submitted[1].Side != clobclient.SideSell {
		t.Fatalf("expected the buy then the sell, got %+v", trader.submitted)
	}
	if trader.submitted[1].Shares != 15.87 {
		t.Fatalf("sell shares = %v, want the settled 15.87", trader.submitted[1].Shares)
	}
	if trader.balanceReads != 4 {
		t.Fatalf("balance read %d times, want 4 (stop once the fill settles)", trader.balanceReads)
	}
}

func TestSellWithoutRecentBuyDoesNotWait(t *testing.T) {
	trader := &fakeTrader{tick: 0.01, balance: "0", settled: "100000000", settleReads: 1}
	f := newFollower(t, trader, false)
	f.Enqueue(activity("SELL", 0.5))
	f.Close()
	if len(trader.submitted) != 0 || trader.balanceReads != 1 {
		t.Fatalf("expected one balance read and no order, got %d reads and %+v", trader.balanceReads, trader.submitted)
	}
}

func TestEnqueueAfterCloseIsRefused(t *testing.T) {
	trader := &fakeTrader{tick: 0.01}
	f := newFollower(t, trader, false)
	f.Close()
	if f.Enqueue(activity("BUY", 0.5)) {
		t.Fatal("enqueue after close was accepted")
	}
	if len(trader.submitted) != 0 {
		t.Fatalf("closed follower submitted %+v", trader.submitted)
	}
}
