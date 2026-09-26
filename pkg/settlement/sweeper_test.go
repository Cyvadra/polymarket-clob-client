package settlement

import (
	"context"
	"errors"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeMarkets struct {
	markets map[string]clobclient.Market
	reads   map[string]int
}

func (f *fakeMarkets) Market(_ context.Context, conditionID string) (*clobclient.Market, error) {
	f.reads[conditionID]++
	m, ok := f.markets[conditionID]
	if !ok {
		return nil, errors.New("no such market")
	}
	return &m, nil
}

type fakeBalances struct {
	tokens map[string]string
	reads  map[string]int
}

func (f *fakeBalances) TokenBalance(_ context.Context, tokenID string) (string, error) {
	f.reads[tokenID]++
	return f.tokens[tokenID], nil
}

type fakeStore struct {
	positions []store.PositionRecord
	settled   map[string]string
	declined  map[string]bool
}

func (f *fakeStore) PositionFeatures(context.Context) ([]store.PositionRecord, error) {
	return f.positions, nil
}

func (f *fakeStore) SettlePosition(_ context.Context, conditionID, tokenID, uniqueTag, shares string) (bool, error) {
	lane := conditionID + "/" + tokenID + "/" + uniqueTag
	if f.declined[lane] {
		return false, nil
	}
	f.settled[lane] = shares
	return true, nil
}

func resolved(winner, loser string) clobclient.Market {
	return clobclient.Market{Closed: true, Tokens: []clobclient.Token{{TokenID: winner, Winner: true}, {TokenID: loser}}}
}

var sweepNow = time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)

// settledAt is a lane that last changed long enough ago to trust the wallet.
var settledAt = sweepNow.Add(-SettleGrace)

func TestSweepEmptiesLanesWorthNothing(t *testing.T) {
	markets := &fakeMarkets{reads: map[string]int{}, markets: map[string]clobclient.Market{
		"lost":     resolved("l2", "l1"),
		"won":      resolved("w1", "w1x"),
		"part":     resolved("w2", "w2x"),
		"redeemed": resolved("w3", "w3x"),
		"closing":  {Closed: true, Tokens: []clobclient.Token{{TokenID: "c1"}, {TokenID: "c2"}}},
		"live":     {Tokens: []clobclient.Token{{TokenID: "o1"}, {TokenID: "o2"}}},
		"selling":  resolved("s2", "s1"),
		"fresh":    resolved("f1", "f1x"),
		"pending":  resolved("p1", "p1x"),
	}}
	balances := &fakeBalances{reads: map[string]int{}, tokens: map[string]string{
		"w1": "10000000", "w2": "4000000", "w3": "0", "f1": "0", "p1": "0",
	}}
	st := &fakeStore{settled: map[string]string{}, declined: map[string]bool{"lost/l1/z": true}, positions: []store.PositionRecord{
		{ConditionID: "lost", TokenID: "l1", UniqueTag: "a", PositionSize: "30", UpdatedAt: settledAt},
		// Declined by the store: a close reserved it after the read.
		{ConditionID: "lost", TokenID: "l1", UniqueTag: "z", PositionSize: "30", UpdatedAt: settledAt},
		{ConditionID: "won", TokenID: "w1", UniqueTag: "b", PositionSize: "10", UpdatedAt: settledAt},
		{ConditionID: "part", TokenID: "w2", UniqueTag: "c", PositionSize: "10", UpdatedAt: settledAt},
		{ConditionID: "redeemed", TokenID: "w3", UniqueTag: "d", PositionSize: "20", UpdatedAt: settledAt},
		{ConditionID: "closing", TokenID: "c1", UniqueTag: "e", PositionSize: "5", UpdatedAt: settledAt},
		{ConditionID: "live", TokenID: "o1", UniqueTag: "f", PositionSize: "5", UpdatedAt: settledAt},
		{ConditionID: "selling", TokenID: "s1", UniqueTag: "g", PositionSize: "5", ReservedSize: "5", UpdatedAt: settledAt},
		// A winner bought just now reads as a zero balance until its tokens land.
		{ConditionID: "fresh", TokenID: "f1", UniqueTag: "i", PositionSize: "5", UpdatedAt: sweepNow.Add(-time.Second)},
		// A winner whose buy has not confirmed on chain, however old, is not
		// trusted to the wallet balance either.
		{ConditionID: "pending", TokenID: "p1", UniqueTag: "j", PositionSize: "5", State: "unsettled", UpdatedAt: settledAt.Add(-time.Hour)},
		{ConditionID: "empty", TokenID: "x", UniqueTag: "h", PositionSize: "0", UpdatedAt: settledAt},
	}}
	resolver, err := NewResolver(markets, balances, func() time.Time { return sweepNow })
	if err != nil {
		t.Fatal(err)
	}
	sweeper, err := NewSweeper(st, resolver, func() time.Time { return sweepNow }, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sweep, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"lost/l1/a":     "0.000000",
		"part/w2/c":     "4.000000",
		"redeemed/w3/d": "0.000000",
	}
	if len(st.settled) != len(want) {
		t.Fatalf("settled=%v, want %v", st.settled, want)
	}
	for lane, shares := range want {
		if st.settled[lane] != shares {
			t.Fatalf("settled=%v, want %v", st.settled, want)
		}
	}
	if sweep != (Sweep{Checked: 10, Lost: 1, Redeemed: 1, Shrunk: 1, Reserved: 2, Settling: 2}) {
		t.Fatalf("sweep=%+v", sweep)
	}
	if markets.reads["empty"] != 0 || balances.reads["l1"] != 0 {
		t.Fatalf("looked up more than it needed: markets=%v balances=%v", markets.reads, balances.reads)
	}
}

// Lanes holding the same winning token share one wallet balance: it is handed
// out in store order, and a lane left alone keeps its claim on it.
func TestSweepSharesAWalletBalanceAcrossLanesOfOneToken(t *testing.T) {
	markets := &fakeMarkets{reads: map[string]int{}, markets: map[string]clobclient.Market{"m": resolved("w", "l")}}
	balances := &fakeBalances{reads: map[string]int{}, tokens: map[string]string{"w": "15000000"}}
	st := &fakeStore{settled: map[string]string{}, positions: []store.PositionRecord{
		{ConditionID: "m", TokenID: "w", UniqueTag: "a", PositionSize: "10", UpdatedAt: settledAt},
		{ConditionID: "m", TokenID: "w", UniqueTag: "b", PositionSize: "4", ReservedSize: "4", UpdatedAt: settledAt},
		{ConditionID: "m", TokenID: "w", UniqueTag: "c", PositionSize: "10", UpdatedAt: settledAt},
		{ConditionID: "m", TokenID: "w", UniqueTag: "d", PositionSize: "10", UpdatedAt: settledAt},
	}}
	resolver, _ := NewResolver(markets, balances, func() time.Time { return sweepNow })
	sweeper, _ := NewSweeper(st, resolver, func() time.Time { return sweepNow }, time.Minute)
	sweep, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// a keeps 10, the reserved b keeps its 4, c gets the last 1, d nothing.
	want := map[string]string{"m/w/c": "1.000000", "m/w/d": "0.000000"}
	if len(st.settled) != len(want) || st.settled["m/w/c"] != want["m/w/c"] || st.settled["m/w/d"] != want["m/w/d"] {
		t.Fatalf("settled=%v, want %v", st.settled, want)
	}
	if sweep != (Sweep{Checked: 4, Redeemed: 1, Shrunk: 1, Reserved: 1}) {
		t.Fatalf("sweep=%+v", sweep)
	}
	if balances.reads["w"] != 1 {
		t.Fatalf("balance reads=%v, want one shared read", balances.reads)
	}
}

func TestResolverAsksAgainOnlyUntilResolved(t *testing.T) {
	now := sweepNow
	markets := &fakeMarkets{reads: map[string]int{}, markets: map[string]clobclient.Market{
		"done":    resolved("w", "l"),
		"pending": {Closed: true, Tokens: []clobclient.Token{{TokenID: "p1"}, {TokenID: "p2"}}},
	}}
	resolver, _ := NewResolver(markets, &fakeBalances{reads: map[string]int{}}, func() time.Time { return now })
	ctx := context.Background()
	for range 2 {
		if outcome, err := resolver.Outcome(ctx, "done", "l"); err != nil || !outcome.Resolved || outcome.Winner {
			t.Fatalf("done: %+v, %v", outcome, err)
		}
		if outcome, err := resolver.Outcome(ctx, "pending", "p1"); err != nil || outcome.Resolved {
			t.Fatalf("pending: %+v, %v", outcome, err)
		}
		now = now.Add(openMarketTTL)
	}
	if markets.reads["done"] != 1 || markets.reads["pending"] != 2 {
		t.Fatalf("market reads=%v", markets.reads)
	}
}

// A token the resolved market does not list says the lane's ids do not
// describe one market; that is an error, never a loss.
func TestResolverRejectsATokenTheMarketDoesNotList(t *testing.T) {
	markets := &fakeMarkets{reads: map[string]int{}, markets: map[string]clobclient.Market{"m": resolved("w", "l")}}
	resolver, _ := NewResolver(markets, &fakeBalances{reads: map[string]int{}}, func() time.Time { return sweepNow })
	if outcome, err := resolver.Outcome(context.Background(), "m", "stranger"); err == nil {
		t.Fatalf("outcome=%+v, want an error", outcome)
	}
}

// A failed lookup is held for failureTTL, so a lane the CLOB cannot serve
// costs one request per interval rather than one per caller.
func TestResolverHoldsAFailedLookup(t *testing.T) {
	now := sweepNow
	markets := &fakeMarkets{reads: map[string]int{}, markets: map[string]clobclient.Market{}}
	resolver, _ := NewResolver(markets, &fakeBalances{reads: map[string]int{}}, func() time.Time { return now })
	ctx := context.Background()
	for range 3 {
		if _, err := resolver.Outcome(ctx, "gone", "t"); err == nil {
			t.Fatal("expected the lookup to fail")
		}
	}
	if markets.reads["gone"] != 1 {
		t.Fatalf("market reads=%v, want the failure held", markets.reads)
	}
	now = now.Add(failureTTL)
	if _, err := resolver.Outcome(ctx, "gone", "t"); err == nil || markets.reads["gone"] != 2 {
		t.Fatalf("err=%v reads=%v, want a retry after failureTTL", err, markets.reads)
	}
}

// Entries nobody has asked about for idleTTL are dropped, so the caches do
// not grow with every market ever traded.
func TestResolverDropsIdleEntries(t *testing.T) {
	now := sweepNow
	markets := &fakeMarkets{reads: map[string]int{}, markets: map[string]clobclient.Market{
		"old": resolved("w", "l"), "new": resolved("w2", "l2"),
	}}
	balances := &fakeBalances{reads: map[string]int{}, tokens: map[string]string{"w": "1000000", "w2": "1000000"}}
	resolver, _ := NewResolver(markets, balances, func() time.Time { return now })
	ctx := context.Background()
	if _, err := resolver.Outcome(ctx, "old", "w"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(idleTTL)
	if _, err := resolver.Outcome(ctx, "new", "w2"); err != nil {
		t.Fatal(err)
	}
	resolver.mu.Lock()
	_, oldMarket := resolver.resolved["old"]
	_, oldBalance := resolver.wallet["w"]
	_, newMarket := resolver.resolved["new"]
	resolver.mu.Unlock()
	if oldMarket || oldBalance || !newMarket {
		t.Fatalf("resolved=%v wallet=%v", resolver.resolved, resolver.wallet)
	}
}

// A winner still in the wallet is reused only within one pass, since
// redemption can move it into cash at any moment and a reused balance would
// count it twice. Once the wallet is empty, the zero is reused for longer.
func TestResolverRereadsAHeldWinnerButReusesAZero(t *testing.T) {
	markets := &fakeMarkets{reads: map[string]int{}, markets: map[string]clobclient.Market{"m": resolved("w", "l")}}
	balances := &fakeBalances{reads: map[string]int{}, tokens: map[string]string{"w": "5000000"}}
	now := sweepNow
	resolver, _ := NewResolver(markets, balances, func() time.Time { return now })
	ctx := context.Background()
	check := func(want float64, reads int) {
		t.Helper()
		outcome, err := resolver.Outcome(ctx, "m", "w")
		if err != nil || outcome.WalletShares != want || balances.reads["w"] != reads {
			t.Fatalf("outcome=%+v err=%v reads=%d, want %v shares after %d reads", outcome, err, balances.reads["w"], want, reads)
		}
	}
	check(5, 1)
	check(5, 1)                // same pass
	balances.tokens["w"] = "0" // redeemed
	now = now.Add(heldBalanceTTL)
	check(0, 2)
	now = now.Add(winnerBalanceTTL / 2)
	check(0, 2)
}
