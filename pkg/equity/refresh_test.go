package equity

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

// steppedBalances answers each read with the next balance; before answers
// the first read, it runs hook (once) while that read is in flight.
type steppedBalances struct {
	balances []string
	calls    atomic.Int32
	during   func()
}

func (s *steppedBalances) BalanceAllowance(context.Context, string, string) (*clobclient.BalanceAllowance, error) {
	n := int(s.calls.Add(1)) - 1
	if n == 0 && s.during != nil {
		s.during()
	}
	if n >= len(s.balances) {
		n = len(s.balances) - 1
	}
	return &clobclient.BalanceAllowance{Balance: s.balances[n]}, nil
}

func TestCashDiscardsReadOvertakenByFill(t *testing.T) {
	balances := &steppedBalances{balances: []string{"5000000", "2000000"}}
	tracker, _ := New(balances, fakePositions{}, fakeQuotes{}, time.Now)
	// A fill lands while the first read is in flight: that read may predate
	// it, so the balance it returns must not be used.
	balances.during = tracker.Invalidate
	cash, _, err := tracker.Cash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cash != 2 {
		t.Fatalf("cash = %v, want the post-fill 2", cash)
	}
	if got := balances.calls.Load(); got != 2 {
		t.Fatalf("reads = %d, want 2", got)
	}
}

func TestRunCashRefreshReadsAfterInvalidate(t *testing.T) {
	balances := &steppedBalances{balances: []string{"1000000", "3000000"}}
	tracker, _ := New(balances, fakePositions{}, fakeQuotes{}, time.Now)
	tracker.SetCashTTL(time.Hour)
	tracker.SetFillSettle(20 * time.Millisecond)
	if _, _, err := tracker.Cash(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tracker.RunCashRefresh(ctx, func(err error) { t.Error(err) })
	tracker.Invalidate()
	deadline := time.Now().Add(2 * time.Second)
	for balances.calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("refresh loop did not read after invalidate")
		}
		time.Sleep(time.Millisecond)
	}
	// The entry that follows is served from the refreshed cache.
	cash, _, err := tracker.Cash(context.Background())
	if err != nil || cash != 3 {
		t.Fatalf("cash = %v, %v; want 3", cash, err)
	}
	if got := balances.calls.Load(); got != 2 {
		t.Fatalf("reads = %d, want 2: the entry should not read itself", got)
	}
}

func TestReadSoonAfterFillIsNotCached(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	balances := &steppedBalances{balances: []string{"1000000", "5000000", "3000000", "9000000"}}
	tracker, _ := New(balances, fakePositions{}, fakeQuotes{}, func() time.Time { return now })
	if _, _, err := tracker.Cash(context.Background()); err != nil {
		t.Fatal(err)
	}
	tracker.Invalidate()
	// Inside the settle window the exchange may still show pre-fill cash:
	// it is used, but the next read goes to the exchange again.
	if cash, _, _ := tracker.Cash(context.Background()); cash != 5 {
		t.Fatalf("cash = %v, want 5", cash)
	}
	now = now.Add(DefaultFillSettle)
	if cash, _, _ := tracker.Cash(context.Background()); cash != 3 {
		t.Fatalf("cash = %v, want the re-read 3", cash)
	}
	// Past the window the read is cached again.
	if cash, _, _ := tracker.Cash(context.Background()); cash != 3 {
		t.Fatalf("cash = %v, want cached 3", cash)
	}
	if got := balances.calls.Load(); got != 3 {
		t.Fatalf("reads = %d, want 3", got)
	}
}
