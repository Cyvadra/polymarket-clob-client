package equity

import (
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeValuer struct {
	snapshot    Snapshot
	invalidated bool
	fresh       bool
	onSnapshot  func()
}

func (f *fakeValuer) Invalidate() { f.invalidated = true }

func (f *fakeValuer) Snapshot(context.Context) (Snapshot, error) {
	f.fresh = f.invalidated
	if f.onSnapshot != nil {
		f.onSnapshot()
	}
	return f.snapshot, nil
}

type fakeEquityStore struct {
	saved     []store.EquitySnapshotRecord
	tradeCash string
	// pendingReads counts PendingTradeCash calls; counted is what each saved
	// snapshot was told it counted.
	pendingReads int
	counted      []store.PendingTradeCash
}

func (f *fakeEquityStore) PendingTradeCash(context.Context) (store.PendingTradeCash, error) {
	f.pendingReads++
	if f.tradeCash == "" {
		return store.PendingTradeCash{USD: "0"}, nil
	}
	return store.PendingTradeCash{USD: f.tradeCash, FillIDs: []string{"fill"}}, nil
}

func (f *fakeEquityStore) RecordEquitySnapshot(_ context.Context, r store.EquitySnapshotRecord, counted store.PendingTradeCash) (store.EquitySnapshotRecord, error) {
	r.ID = int64(len(f.saved) + 1)
	f.saved = append(f.saved, r)
	f.counted = append(f.counted, counted)
	return r, nil
}

func (f *fakeEquityStore) LatestEquitySnapshot(context.Context) (store.EquitySnapshotRecord, bool, error) {
	if len(f.saved) == 0 {
		return store.EquitySnapshotRecord{}, false, nil
	}
	return f.saved[len(f.saved)-1], true, nil
}

func (f *fakeEquityStore) LatestEquitySnapshotWithReason(_ context.Context, reasons ...string) (store.EquitySnapshotRecord, bool, error) {
	for i := len(f.saved) - 1; i >= 0; i-- {
		if slices.Contains(reasons, f.saved[i].Reason) {
			return f.saved[i], true, nil
		}
	}
	return store.EquitySnapshotRecord{}, false, nil
}

func (f *fakeEquityStore) ExternalFlowAfter(_ context.Context, id int64) (string, error) {
	var flow float64
	for _, r := range f.saved {
		if r.ID > id && r.ExternalFlowUSD != "" {
			v, _ := strconv.ParseFloat(r.ExternalFlowUSD, 64)
			flow += v
		}
	}
	return strconv.FormatFloat(flow, 'f', -1, 64), nil
}

func (f *fakeEquityStore) PeakTradeIndexSince(_ context.Context, since time.Time) (string, bool, error) {
	peak, ok := 0.0, false
	for i, r := range f.saved {
		// Snapshots before since count only as the last one before it.
		if r.TakenAt.Before(since) && i+1 < len(f.saved) && f.saved[i+1].TakenAt.Before(since) {
			continue
		}
		v, _ := strconv.ParseFloat(r.TradeIndex, 64)
		if !ok || v > peak {
			peak, ok = v, true
		}
	}
	return strconv.FormatFloat(peak, 'f', -1, 64), ok, nil
}

var recordStart = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

// recordAt values the wallet at cash + positions, with tradeCash of fills and
// payouts since the previous snapshot, and returns the saved row.
func recordAt(t *testing.T, r *Recorder, s *fakeEquityStore, wallet *fakeValuer, minute int, cash, positions float64, tradeCash string) store.EquitySnapshotRecord {
	t.Helper()
	at := recordStart.Add(time.Duration(minute) * time.Minute)
	wallet.snapshot = Snapshot{CashUSD: cash, PositionsUSD: positions, EquityUSD: cash + positions, CashAsOf: at, AsOf: at}
	s.tradeCash = tradeCash
	saved, err := r.Record(context.Background(), "settlement")
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func index(t *testing.T, r store.EquitySnapshotRecord) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(r.TradeIndex, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func newTestRecorder() (*Recorder, *fakeEquityStore, *fakeValuer) {
	s, wallet := &fakeEquityStore{}, &fakeValuer{}
	return &Recorder{wallet: wallet, store: s, flowThreshold: DefaultFlowThresholdUSD}, s, wallet
}

func TestRecordValuesAfreshAndStartsTheIndexAtOne(t *testing.T) {
	r, s, wallet := newTestRecorder()
	got := recordAt(t, r, s, wallet, 0, 40, 60, "")
	if !wallet.fresh {
		t.Fatal("cash cache was not invalidated before valuing")
	}
	if got.CashUSD != "40.000000" || got.EquityUSD != "100.000000" || got.ExternalFlowUSD != "0.000000" || index(t, got) != 1 || !got.TakenAt.Equal(recordStart) {
		t.Fatalf("unexpected first record %+v", got)
	}
}

func TestRecordLeavesDepositsAndWithdrawalsOutOfTheIndex(t *testing.T) {
	r, s, wallet := newTestRecorder()
	recordAt(t, r, s, wallet, 0, 100, 0, "")
	// A $1000 deposit and nothing traded: cash up, index unchanged.
	deposit := recordAt(t, r, s, wallet, 1, 1100, 0, "0")
	if deposit.ExternalFlowUSD != "1000.000000" || index(t, deposit) != 1 {
		t.Fatalf("deposit read as trading: %+v", deposit)
	}
	// Bought $110 of shares now worth $99: a 1% trading loss on $1100.
	loss := recordAt(t, r, s, wallet, 2, 990, 99, "-110")
	if loss.ExternalFlowUSD != "0.000000" || math.Abs(index(t, loss)-0.99) > 1e-9 {
		t.Fatalf("loss not measured on the capital it was made on: %+v", loss)
	}
	// Withdrew $500 while the shares redeemed for $110: a gain of $11 on $589.
	gain := recordAt(t, r, s, wallet, 3, 600, 0, "110")
	if gain.ExternalFlowUSD != "-500.000000" || math.Abs(index(t, gain)-0.99*(1+11.0/589)) > 1e-9 {
		t.Fatalf("withdrawal read as trading: %+v", gain)
	}
}

func TestRecordKeepsSmallUnexplainedCashInTheIndex(t *testing.T) {
	r, s, wallet := newTestRecorder()
	recordAt(t, r, s, wallet, 0, 100, 0, "")
	// A $0.50 rebate no fill recorded is below the threshold: it is trading.
	got := recordAt(t, r, s, wallet, 1, 100.5, 0, "0")
	if got.ExternalFlowUSD != "0.000000" || math.Abs(index(t, got)-1.005) > 1e-9 {
		t.Fatalf("small residual read as a flow: %+v", got)
	}
}

func TestRecordReadsTradeCashBeforeTheBalanceAndCountsIt(t *testing.T) {
	r, s, wallet := newTestRecorder()
	recordAt(t, r, s, wallet, 0, 100, 0, "")
	// A fill stored after the pending read is not in it, so the balance
	// must be read after it: every counted fill has then moved the cash.
	wallet.onSnapshot = func() {
		if s.pendingReads != 2 {
			t.Fatalf("balance read after %d pending trade cash reads, want 2", s.pendingReads)
		}
	}
	got := recordAt(t, r, s, wallet, 1, 60, 50, "-40")
	if got.TradeCashUSD != "-40.000000" || len(s.counted[1].FillIDs) != 1 {
		t.Fatalf("pending trade cash not counted by the snapshot: %+v, %+v", got, s.counted[1])
	}
}

func TestRecordSkipsIncompleteValuation(t *testing.T) {
	r, s, wallet := newTestRecorder()
	wallet.snapshot = Snapshot{EquityUSD: 10, LookupFailures: 1}
	if _, err := r.Record(context.Background(), "settlement"); !errors.Is(err, ErrIncompleteValuation) {
		t.Fatalf("expected ErrIncompleteValuation, got %v", err)
	}
	if len(s.saved) != 0 {
		t.Fatal("an incomplete valuation was saved")
	}
}

func TestDrawdown(t *testing.T) {
	for _, c := range []struct{ peak, value, want float64 }{
		{1.2, 0.9, 0.25},
		{1, 1, 0},
		{1, 1.1, 0},
		{0, 0.5, 0},
	} {
		if got := Drawdown(c.peak, c.value); math.Abs(got-c.want) > 1e-12 {
			t.Errorf("Drawdown(%g, %g) = %g, want %g", c.peak, c.value, got, c.want)
		}
	}
}

func TestDrawdownGuardMeasuresFromTheStart(t *testing.T) {
	r, s, wallet := newTestRecorder()
	// Before the start: a peak the limit must not see, then a fall back to
	// the level trading starts from.
	recordAt(t, r, s, wallet, -3, 100, 0, "")
	recordAt(t, r, s, wallet, -2, 200, 0, "100")  // index 2
	recordAt(t, r, s, wallet, -1, 100, 0, "-100") // index 1
	guard, err := NewDrawdownGuard(s, recordStart, 0.1)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if drawdown, err := guard.Refresh(ctx); err != nil || drawdown != 0 {
		t.Fatalf("no snapshot since the start: drawdown %g, %v", drawdown, err)
	}
	recordAt(t, r, s, wallet, 0, 110, 0, "10") // the first peak since the start
	recordAt(t, r, s, wallet, 1, 104, 0, "-6") // 5.5% below it
	if _, err := guard.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if suspended, _ := guard.Suspended(); suspended {
		t.Fatal("suspended below the limit")
	}
	// A withdrawal is no drawdown; a further $20 trading loss is.
	recordAt(t, r, s, wallet, 2, 54, 0, "0")
	if _, err := guard.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if suspended, _ := guard.Suspended(); suspended {
		t.Fatal("suspended by a withdrawal")
	}
	recordAt(t, r, s, wallet, 3, 34, 0, "-20")
	drawdown, err := guard.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	suspended, reason := guard.Suspended()
	if !suspended || drawdown <= 0.1 || reason == "" {
		t.Fatalf("expected a suspension, drawdown %g, reason %q", drawdown, reason)
	}
}

func TestDrawdownGuardMeasuresALossAcrossTheStart(t *testing.T) {
	r, s, wallet := newTestRecorder()
	recordAt(t, r, s, wallet, -1, 100, 0, "")
	// The first snapshot after the start is already 30% below the level
	// trading stood at when the start came.
	recordAt(t, r, s, wallet, 0, 70, 0, "-30")
	guard, err := NewDrawdownGuard(s, recordStart, 0.1)
	if err != nil {
		t.Fatal(err)
	}
	drawdown, err := guard.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if suspended, _ := guard.Suspended(); !suspended || math.Abs(drawdown-0.3) > 1e-9 {
		t.Fatalf("loss across the start not measured: drawdown %g, suspended %v", drawdown, suspended)
	}
}
