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

type fakeCommitments struct{ openBuys, splitting string }

func (f fakeCommitments) OpenBuyNotional(context.Context) (string, error)   { return f.openBuys, nil }
func (f fakeCommitments) InFlightSplitCash(context.Context) (string, error) { return f.splitting, nil }

// A split in flight still sits in the CLOB's cash until it lands: entries
// must not size against it.
func TestSizeCountsSplitsInFlight(t *testing.T) {
	sizer := newTestSizer(t, "10000000", "0")
	sizer.openBuys = fakeCommitments{openBuys: "0", splitting: "6"}
	// $10 cash less $6 splitting leaves $4; an 8% entry of $60 equity is $4.80.
	if _, err := sizer.Size(context.Background(), "0.08"); !errors.Is(err, ErrInsufficientCash) {
		t.Fatalf("entry against a split's cash: err=%v, want insufficient cash", err)
	}
}

// A split claims free cash through the sizer, so an entry cannot take the
// same dollars while the split is being recorded, and the claim gives way
// once it is.
func TestClaimHoldsFreeCashForASplit(t *testing.T) {
	sizer := newTestSizer(t, "10000000", "4")
	granted, claim, err := sizer.Claim(context.Background(), 15)
	if err != nil || granted != 6 {
		t.Fatalf("claim: granted %v err %v, want the $6 free", granted, err)
	}
	if _, err := sizer.Size(context.Background(), "0.01"); !errors.Is(err, ErrInsufficientCash) {
		t.Fatalf("entry during a claim: err=%v, want insufficient cash", err)
	}
	claim.Release()
	if granted, claim, err := sizer.Claim(context.Background(), 2); err != nil || granted != 2 {
		t.Fatalf("claim under free cash: granted %v err %v, want 2", granted, err)
	} else {
		claim.Release()
	}
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

// releasingOpenBuys answers the first read with the working buys as they were
// before an entry's reservation landed, releasing that entry mid-read as its
// reservation commits. Later reads see the reservation.
type releasingOpenBuys struct {
	before, after string
	release       func()
	reads         int
}

func (r *releasingOpenBuys) OpenBuyNotional(context.Context) (string, error) {
	r.reads++
	if r.reads == 1 {
		r.release()
		return r.before, nil
	}
	return r.after, nil
}

// Free cash is read outside the lock, so an entry released during the read is
// in neither pending nor the stale read; Size must read again, or it counts
// the same cash twice.
func TestSizeRereadsWhenAnEntryIsReleasedMidRead(t *testing.T) {
	// $6 cash + $50 of shares = $56 equity; 8% entries cost $4.48, so only
	// one fits in the $6 free.
	sizer := newTestSizer(t, "6000000", "0")
	first, err := sizer.Size(context.Background(), "0.08")
	if err != nil {
		t.Fatal(err)
	}
	sizer.openBuys = &releasingOpenBuys{before: "0", after: first.TargetUSD, release: first.Release}
	if _, err := sizer.Size(context.Background(), "0.08"); !errors.Is(err, ErrInsufficientCash) {
		t.Fatalf("second entry err=%v, want insufficient cash: the first entry's buy still holds its cash", err)
	}
}
