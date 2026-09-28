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

func TestSizeFromLossAnchorIgnoresLiveEquity(t *testing.T) {
	// Live equity is $100 ($50 cash + $50 of shares); the anchor says $40.
	sizer := newTestSizer(t, "50000000", "0")
	snapshots := &fakeEquityStore{}
	anchor, err := NewLossAnchor(snapshots)
	if err != nil {
		t.Fatal(err)
	}
	sizer.SetLossAnchor(anchor)
	// Before any base, live equity ($100) is used.
	if entry, err := sizer.Size(context.Background(), "0.1"); err != nil || entry.TargetUSD != "10" {
		t.Fatalf("before any base: entry=%+v err=%v", entry, err)
	} else {
		entry.Release()
	}

	record := func(reason, equity string) {
		snapshots.saved = append(snapshots.saved, store.EquitySnapshotRecord{ID: int64(len(snapshots.saved) + 1), Reason: reason, EquityUSD: equity})
		if _, _, err := anchor.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	size := func() Entry {
		t.Helper()
		entry, err := sizer.Size(context.Background(), "0.1")
		if err != nil {
			t.Fatal(err)
		}
		entry.Release()
		return entry
	}
	record(ReasonSizingBase, "40")
	if entry := size(); entry.TargetUSD != "4" || entry.EquityUSD != 40 {
		t.Fatalf("from base: %+v", entry)
	}
	// Profitable settlements do not move the base.
	record(ReasonSettlement, "90")
	if entry := size(); entry.TargetUSD != "4" {
		t.Fatalf("after a win: %+v", entry)
	}
	// A loss resets it to the equity recorded after the loss.
	record(ReasonSettlementLoss, "70")
	if entry := size(); entry.TargetUSD != "7" || entry.EquityUSD != 70 {
		t.Fatalf("after a loss: %+v", entry)
	}
	// A deposit, then a withdrawal, move it by their amount.
	flow := func(usd string) {
		snapshots.saved = append(snapshots.saved, store.EquitySnapshotRecord{ID: int64(len(snapshots.saved) + 1), Reason: ReasonSettlement, EquityUSD: "999", ExternalFlowUSD: usd})
		if _, _, err := anchor.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	flow("30")
	if entry := size(); entry.TargetUSD != "10" {
		t.Fatalf("after a $30 deposit: %+v", entry)
	}
	flow("-50")
	if entry := size(); entry.TargetUSD != "5" {
		t.Fatalf("after a $50 withdrawal: %+v", entry)
	}
	// Withdrawing more than the base leaves nothing to size from.
	flow("-100")
	if _, err := sizer.Size(context.Background(), "0.1"); !errors.Is(err, ErrInsufficientCash) {
		t.Fatalf("base below zero: err=%v", err)
	}
	// The next loss anchors afresh; flows before it are in its equity.
	record(ReasonSettlementLoss, "60")
	if entry := size(); entry.TargetUSD != "6" {
		t.Fatalf("after the next loss: %+v", entry)
	}
}

func TestSizeFromLossAnchorStillChecksLiveCash(t *testing.T) {
	// $5 cash; an anchored $100 at 10% needs $10.
	sizer := newTestSizer(t, "5000000", "0")
	anchor, err := NewLossAnchor(&fakeEquityStore{saved: []store.EquitySnapshotRecord{{ID: 1, Reason: ReasonSettlementLoss, EquityUSD: "100"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := anchor.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	sizer.SetLossAnchor(anchor)
	if _, err := sizer.Size(context.Background(), "0.1"); !errors.Is(err, ErrInsufficientCash) {
		t.Fatalf("err=%v, want insufficient cash", err)
	}
}
