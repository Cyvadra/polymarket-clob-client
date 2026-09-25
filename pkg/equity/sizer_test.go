package equity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeOpenBuys string

func (f fakeOpenBuys) OpenBuyNotional(context.Context) (string, error) { return string(f), nil }

func newTestSizer(t *testing.T, cash string, openBuys string) *Sizer {
	t.Helper()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	positions := fakePositions{{ConditionID: "c1", TokenID: "up1", PositionSize: "100", EntryPrice: "0.4"}}
	quotes := fakeQuotes{"c1": {ConditionID: "c1", At: now, Up: marketquotes.Quote{AssetID: "up1", Bid: 0.5}}}
	tracker, err := New(&fakeBalances{balance: cash}, positions, quotes, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sizer, err := NewSizer(tracker, fakeOpenBuys(openBuys))
	if err != nil {
		t.Fatal(err)
	}
	return sizer
}

func TestSizeUsesEquityNotCash(t *testing.T) {
	// $50 cash + 100 shares at 0.5 bid = $100 equity.
	sizer := newTestSizer(t, "50000000", "0")
	entry, err := sizer.Size(context.Background(), "0.03")
	if err != nil {
		t.Fatal(err)
	}
	defer entry.Release()
	if entry.TargetUSD != "3" || !near(entry.EquityUSD, 100) {
		t.Fatalf("entry=%+v", entry)
	}
}

func TestSizeHoldsFreeCashUntilRelease(t *testing.T) {
	// $10 cash + $50 of shares = $60 equity; $4 in working buys leaves $6
	// free, and 8% entries cost $4.80 each.
	sizer := newTestSizer(t, "10000000", "4")
	first, err := sizer.Size(context.Background(), "0.08")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sizer.Size(context.Background(), "0.08"); !errors.Is(err, ErrInsufficientCash) {
		t.Fatalf("second entry err=%v, want insufficient cash", err)
	}
	first.Release()
	first.Release() // idempotent
	second, err := sizer.Size(context.Background(), "0.08")
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	second.Release()
	if sizer.pending != 0 {
		t.Fatalf("pending=%v", sizer.pending)
	}
}

func TestSizeRejectsBadFractions(t *testing.T) {
	sizer := newTestSizer(t, "50000000", "0")
	for _, fraction := range []string{"0", "-0.1", "3", "abc", "NaN", "0.2500001"} {
		if _, err := sizer.Size(context.Background(), fraction); !errors.Is(err, ErrInvalidFraction) {
			t.Fatalf("fraction %q err=%v", fraction, err)
		}
	}
	sizer.SetMaxFraction(0.5)
	entry, err := sizer.Size(context.Background(), "0.4")
	if err != nil {
		t.Fatalf("0.4 under raised max: %v", err)
	}
	entry.Release()
}

var _ store.PositionStore = fakePositions{}
