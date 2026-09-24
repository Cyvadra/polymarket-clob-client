package copytrading

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

type fakeTrader struct {
	tick, minSize float64
	balance       string
	submitted     []clobclient.UserOrder
}

func (f *fakeTrader) TickSize(context.Context, string) (float64, error)     { return f.tick, nil }
func (f *fakeTrader) MinOrderSize(context.Context, string) (float64, error) { return f.minSize, nil }

func (f *fakeTrader) SubmitOrder(_ context.Context, order clobclient.UserOrder) (*clobclient.OrderResponse, error) {
	f.submitted = append(f.submitted, order)
	return &clobclient.OrderResponse{Success: true, OrderID: fmt.Sprint(len(f.submitted)), Status: "matched"}, nil
}

func (f *fakeTrader) BalanceAllowance(context.Context, string, string) (*clobclient.BalanceAllowance, error) {
	return &clobclient.BalanceAllowance{Balance: f.balance}, nil
}

var now = time.UnixMilli(1_790_000_000_000)

func newFollower(t *testing.T, trader *fakeTrader, dryRun bool) *Follower {
	t.Helper()
	f, err := New(Config{USD: 10, InitialDiff: 0.02, MaxTradeAge: 5 * time.Second, DryRun: dryRun, Now: func() time.Time { return now }}, trader)
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
