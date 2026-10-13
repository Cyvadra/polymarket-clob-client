package equity

import (
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
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

func (f *fakeValuer) SnapshotWithStrategies(ctx context.Context) (Snapshot, error) {
	return f.Snapshot(ctx)
}

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
	lost      bool
	// byStrategy is the split of tradeCash the next PendingTradeCash reports.
	byStrategy map[string]string
	// pendingReads counts PendingTradeCash calls; counted is what each saved
	// snapshot was told it counted.
	pendingReads int
	counted      []store.PendingTradeCash
}

func (f *fakeEquityStore) PendingTradeCash(context.Context) (store.PendingTradeCash, error) {
	f.pendingReads++
	if f.tradeCash == "" {
		return store.PendingTradeCash{USD: "0", Lost: f.lost, ByStrategy: f.byStrategy}, nil
	}
	return store.PendingTradeCash{USD: f.tradeCash, FillIDs: []string{"fill"}, Lost: f.lost, ByStrategy: f.byStrategy}, nil
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

func (f *fakeEquityStore) StrategyEquity(_ context.Context, id int64) ([]store.StrategyEquityRecord, error) {
	return f.saved[id-1].Strategies, nil
}

func (f *fakeEquityStore) PeakStrategyIndexSince(_ context.Context, strategy string, since time.Time) (string, bool, error) {
	peak, ok := 0.0, false
	var before *store.StrategyEquityRecord
	for _, r := range f.saved {
		for _, part := range r.Strategies {
			if part.Strategy != strategy {
				continue
			}
			if r.TakenAt.Before(since) {
				before = &part
				continue
			}
			v, _ := strconv.ParseFloat(part.TradeIndex, 64)
			if !ok || v > peak {
				peak, ok = v, true
			}
		}
	}
	if before != nil {
		v, _ := strconv.ParseFloat(before.TradeIndex, 64)
		if !ok || v > peak {
			peak, ok = v, true
		}
	} else if ok {
		// First seen after since: it stood at 1 before its first interval.
		peak = max(peak, 1)
	}
	return strconv.FormatFloat(peak, 'f', -1, 64), ok, nil
}

// TradingPnLSince lists the saved intervals' dollars; the Postgres store's
// test covers the rules for gaps in a strategy's split.
func (f *fakeEquityStore) TradingPnLSince(_ context.Context, strategy string, since time.Time, snapshotID int64) ([]store.TradingPnL, bool, error) {
	num := func(text string) float64 { v, _ := strconv.ParseFloat(text, 64); return v }
	var intervals []store.TradingPnL
	for i, r := range f.saved {
		var pnl float64
		part, found := strategyPart(r.Strategies, strategy)
		switch {
		case strategy != "" && !found:
			if r.ID == snapshotID {
				return nil, false, nil
			}
			continue
		case i == 0:
		case strategy == "":
			pnl = num(r.EquityUSD) - num(f.saved[i-1].EquityUSD) - num(r.ExternalFlowUSD)
		default:
			prior, _ := strategyPart(f.saved[i-1].Strategies, strategy)
			pnl = num(part.TradeCashUSD) + num(part.PositionsUSD) - num(prior.PositionsUSD)
		}
		if !r.TakenAt.Before(since) {
			intervals = append(intervals, store.TradingPnL{SnapshotID: r.ID,
				PnLUSD: strconv.FormatFloat(pnl, 'f', -1, 64), ExternalFlowUSD: r.ExternalFlowUSD})
		}
		if r.ID == snapshotID {
			return intervals, !r.TakenAt.Before(since), nil
		}
	}
	return nil, false, nil
}

func strategyPart(parts []store.StrategyEquityRecord, strategy string) (store.StrategyEquityRecord, bool) {
	for _, part := range parts {
		if part.Strategy == strategy {
			return part, true
		}
	}
	return store.StrategyEquityRecord{PositionsUSD: "0"}, false
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

// A loss the store has not counted yet tags the snapshot, whatever reason
// the caller gave, so one swept before a restart still resets the sizing base.
func TestRecordTagsAPendingLoss(t *testing.T) {
	r, s, wallet := newTestRecorder()
	s.lost = true
	for _, reason := range []string{ReasonSettlement, ReasonSizingBase} {
		wallet.snapshot = Snapshot{CashUSD: 40, PositionsUSD: 60, EquityUSD: 100, CashAsOf: recordStart}
		got, err := r.Record(context.Background(), reason)
		if err != nil {
			t.Fatal(err)
		}
		if got.Reason != ReasonSettlementLoss {
			t.Fatalf("reason %q recorded as %q, want %q", reason, got.Reason, ReasonSettlementLoss)
		}
	}
	s.lost = false
	got, err := r.Record(context.Background(), ReasonSettlement)
	if err != nil || got.Reason != ReasonSettlement {
		t.Fatalf("no pending loss: reason %q err %v", got.Reason, err)
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

// A transfer in or out with nothing settling still has to reach the sizing
// base, so the wallet notices cash trading does not explain.
func TestCashMovedByTransfersOnly(t *testing.T) {
	r, s, wallet := newTestRecorder()
	ctx := context.Background()
	if moved, err := r.CashMoved(ctx); err != nil || moved {
		t.Fatalf("no snapshot yet: moved=%v err=%v", moved, err)
	}
	recordAt(t, r, s, wallet, 0, 100, 0, "")
	for _, c := range []struct {
		cash      float64
		tradeCash string
		want      bool
	}{
		{101.1, "", true},   // a $1.10 transfer in
		{98.9, "", true},    // a $1.10 transfer out
		{100.5, "", false},  // a $0.50 rebate stays trading
		{110, "10", false},  // a payout the store recorded
		{111.1, "10", true}, // a payout and a transfer
	} {
		wallet.snapshot = Snapshot{CashUSD: c.cash, EquityUSD: c.cash}
		s.tradeCash = c.tradeCash
		if moved, err := r.CashMoved(ctx); err != nil || moved != c.want {
			t.Fatalf("cash %v trade cash %q: moved=%v err=%v, want %v", c.cash, c.tradeCash, moved, err, c.want)
		}
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
	guard, err := NewDrawdownGuard(s, recordStart, FractionLimit(0.1))
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

func TestDrawdownGuardDollarLimit(t *testing.T) {
	r, s, wallet := newTestRecorder()
	recordAt(t, r, s, wallet, -1, 100, 0, "")
	guard, err := NewDrawdownGuard(s, recordStart, DollarLimit(25))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	recordAt(t, r, s, wallet, 0, 80, 0, "-20") // $20 down
	if _, err := guard.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if suspended, reason := guard.Suspended(); suspended {
		t.Fatalf("suspended $20 down on a $25 limit: %s", reason)
	}
	recordAt(t, r, s, wallet, 1, 70, 0, "-10") // $30 down
	if _, err := guard.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if suspended, reason := guard.Suspended(); !suspended || !strings.Contains(reason, "$30.00") || !strings.Contains(reason, "$25 limit") {
		t.Fatalf("expected a $30 suspension, got %v %q", suspended, reason)
	}
	// A deposit raises equity but is no trading profit, so it neither adds to
	// the dollars lost nor hides them.
	recordAt(t, r, s, wallet, 2, 1070, 0, "0")
	if _, err := guard.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if suspended, reason := guard.Suspended(); !suspended || !strings.Contains(reason, "$30.00") {
		t.Fatalf("a deposit changed the dollar drawdown: %v %q", suspended, reason)
	}
	for _, bad := range []DrawdownLimit{{}, FractionLimit(1), DollarLimit(-1), DollarLimit(0), DollarLimit(math.Inf(1)), DollarLimit(math.NaN()), FractionLimit(math.NaN())} {
		if _, err := NewDrawdownGuard(s, recordStart, bad); err == nil {
			t.Errorf("limit %+v accepted", bad)
		}
	}
}

func TestDrawdownGuardMeasuresALossAcrossTheStart(t *testing.T) {
	r, s, wallet := newTestRecorder()
	recordAt(t, r, s, wallet, -1, 100, 0, "")
	// The first snapshot after the start is already 30% below the level
	// trading stood at when the start came.
	recordAt(t, r, s, wallet, 0, 70, 0, "-30")
	guard, err := NewDrawdownGuard(s, recordStart, FractionLimit(0.1))
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

// recordSplit is recordAt with the positions and trade cash split by strategy.
func recordSplit(t *testing.T, r *Recorder, s *fakeEquityStore, wallet *fakeValuer, minute int, cash float64, positions map[string]float64, tradeCash map[string]float64) store.EquitySnapshotRecord {
	t.Helper()
	at := recordStart.Add(time.Duration(minute) * time.Minute)
	var held, moved float64
	for _, usd := range positions {
		held += usd
	}
	s.byStrategy = map[string]string{}
	for name, usd := range tradeCash {
		moved += usd
		s.byStrategy[name] = strconv.FormatFloat(usd, 'f', -1, 64)
	}
	wallet.snapshot = Snapshot{CashUSD: cash, PositionsUSD: held, EquityUSD: cash + held, PositionsByStrategy: positions, CashAsOf: at, AsOf: at}
	s.tradeCash = strconv.FormatFloat(moved, 'f', -1, 64)
	saved, err := r.Record(context.Background(), "settlement")
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func strategyIndex(t *testing.T, r store.EquitySnapshotRecord, strategy string) float64 {
	t.Helper()
	for _, part := range r.Strategies {
		if part.Strategy == strategy {
			v, err := strconv.ParseFloat(part.TradeIndex, 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatalf("no %q in %+v", strategy, r.Strategies)
	return 0
}

func TestRecordSplitsTheTradeIndexByStrategy(t *testing.T) {
	r, s, wallet := newTestRecorder()
	// $100 cash; a buys $20, b buys $30.
	recordSplit(t, r, s, wallet, 0, 50, map[string]float64{"a": 20, "b": 30}, map[string]float64{"a": -20, "b": -30})
	// a's lane falls to $10; b sells for $40. Equity 100 -> 100.
	got := recordSplit(t, r, s, wallet, 1, 90, map[string]float64{"a": 10}, map[string]float64{"b": 40})
	if a, b := strategyIndex(t, got, "a"), strategyIndex(t, got, "b"); math.Abs(a-0.9) > 1e-9 || math.Abs(b-1.1) > 1e-9 {
		t.Fatalf("a %g, b %g", a, b)
	}
	if index(t, got) != 1 {
		t.Fatalf("wallet index %g", index(t, got))
	}
	// A $100 deposit changes neither; a's lane then loses its last $10, now
	// over $200 of equity.
	recordSplit(t, r, s, wallet, 2, 190, map[string]float64{"a": 10}, nil)
	got = recordSplit(t, r, s, wallet, 3, 190, map[string]float64{"a": 0}, nil)
	if a, b := strategyIndex(t, got, "a"), strategyIndex(t, got, "b"); math.Abs(a-0.9*0.95) > 1e-9 || math.Abs(b-1.1) > 1e-9 {
		t.Fatalf("after deposit: a %g, b %g", a, b)
	}
}

func TestStrategyDrawdownCountsTheFirstIntervalsLoss(t *testing.T) {
	r, s, wallet := newTestRecorder()
	recordSplit(t, r, s, wallet, -1, 100, nil, nil)
	// a opens and is down 15% of the wallet in its first interval.
	recordSplit(t, r, s, wallet, 0, 80, map[string]float64{"a": 5}, map[string]float64{"a": -20})
	guard, err := NewStrategyDrawdownGuard(s, "a", recordStart, FractionLimit(0.1))
	if err != nil {
		t.Fatal(err)
	}
	if drawdown, err := guard.Refresh(context.Background()); err != nil || math.Abs(drawdown-0.15) > 1e-9 {
		t.Fatalf("drawdown %g, %v; want 0.15", drawdown, err)
	}
	if suspended, _ := guard.Suspended(); !suspended {
		t.Fatal("a first-interval loss over the limit did not suspend")
	}
}

func TestDrawdownLimitsHoldBackOnlyTheStrategyOverItsLimit(t *testing.T) {
	r, s, wallet := newTestRecorder()
	recordSplit(t, r, s, wallet, 0, 50, map[string]float64{"a": 20, "b": 30}, map[string]float64{"a": -20, "b": -30})
	recordSplit(t, r, s, wallet, 1, 90, map[string]float64{"a": 5}, map[string]float64{"b": 40})
	walletGuard, err := NewDrawdownGuard(s, recordStart, FractionLimit(0.2))
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewStrategyDrawdownGuard(s, "a", recordStart, FractionLimit(0.1))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewStrategyDrawdownGuard(s, "b", recordStart, FractionLimit(0.1))
	if err != nil {
		t.Fatal(err)
	}
	limits := NewDrawdownLimits(walletGuard, a, b)
	for _, guard := range limits.Guards() {
		if _, err := guard.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if suspended, reason := limits.Suspended("a"); !suspended || reason == "" {
		t.Fatal("a is 15% down and not held back")
	}
	if suspended, _ := limits.Suspended("b"); suspended {
		t.Fatal("b is held back by a's loss")
	}
	if suspended, _ := limits.Suspended("c"); suspended {
		t.Fatal("a strategy with no limit is held back")
	}
}

func TestRecordKeepsUnattributedLanesUnderTheEmptyName(t *testing.T) {
	r, s, wallet := newTestRecorder()
	// Held lanes with no recorded strategy still leave a row, so the next
	// interval is not mistaken for one that predates the split.
	first := recordSplit(t, r, s, wallet, 0, 50, map[string]float64{"": 50}, map[string]float64{"": -50})
	if len(first.Strategies) != 1 || first.Strategies[0].Strategy != "" {
		t.Fatalf("unattributed lane not recorded: %+v", first.Strategies)
	}
	// The lane loses $10 while a strategy opens its first lane at a loss.
	got := recordSplit(t, r, s, wallet, 1, 50, map[string]float64{"": 40, "a": 0}, map[string]float64{"a": 0})
	if u := strategyIndex(t, got, ""); math.Abs(u-0.9) > 1e-9 {
		t.Fatalf("unattributed index %g", u)
	}
}

func TestRecordSkipsTheIntervalAfterASnapshotWithoutASplit(t *testing.T) {
	r, s, wallet := newTestRecorder()
	// A snapshot recorded before the split existed holds positions and no rows.
	recordAt(t, r, s, wallet, 0, 50, 50, "")
	got := recordSplit(t, r, s, wallet, 1, 50, map[string]float64{"a": 40}, nil)
	if a := strategyIndex(t, got, "a"); a != 1 {
		t.Fatalf("interval over an unknown split counted: %g", a)
	}
}

func TestRecordFailsOnUnreadablePreviousPositions(t *testing.T) {
	r, s, wallet := newTestRecorder()
	recordAt(t, r, s, wallet, 0, 50, 50, "")
	s.saved[len(s.saved)-1].PositionsUSD = "oops"
	wallet.snapshot = Snapshot{CashUSD: 50, PositionsUSD: 40, EquityUSD: 90, PositionsByStrategy: map[string]float64{"a": 40}, AsOf: recordStart.Add(time.Minute)}
	if _, err := r.Record(context.Background(), "settlement"); err == nil {
		t.Fatal("expected an error for unreadable previous positions")
	}
}

func TestDrawdownLimitStringIsAsConfigured(t *testing.T) {
	for limit, want := range map[DrawdownLimit]string{FractionLimit(7.0 / 100): "7%", FractionLimit(0.125): "12.5%", DollarLimit(500): "$500", DollarLimit(12.5): "$12.5", FractionLimit(1e-7): "0.00001%"} {
		if got := limit.String(); got != want {
			t.Errorf("%+v formats as %q, want %q", limit, got, want)
		}
	}
}

func TestStrategyDollarDrawdownIgnoresOtherStrategiesGains(t *testing.T) {
	r, s, wallet := newTestRecorder()
	recordSplit(t, r, s, wallet, -1, 100, nil, nil)
	recordSplit(t, r, s, wallet, 0, 85, nil, map[string]float64{"a": -15})
	// b more than doubles the wallet; a's loss is still $15.
	recordSplit(t, r, s, wallet, 1, 985, nil, map[string]float64{"b": 900})
	ctx := context.Background()
	for limit, want := range map[float64]bool{20: false, 10: true} {
		guard, err := NewStrategyDrawdownGuard(s, "a", recordStart, DollarLimit(limit))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := guard.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		if suspended, reason := guard.Suspended(); suspended != want || (want && !strings.Contains(reason, "$15.00")) {
			t.Fatalf("$%g limit: suspended %v %q, want %v at $15.00", limit, suspended, reason, want)
		}
	}
}

func TestDollarDrawdownForgetsASpikeAFlowReverses(t *testing.T) {
	pnl := func(rows ...[2]string) []store.TradingPnL {
		var intervals []store.TradingPnL
		for i, row := range rows {
			intervals = append(intervals, store.TradingPnL{SnapshotID: int64(i + 1), PnLUSD: row[0], ExternalFlowUSD: row[1]})
		}
		return intervals
	}
	for name, tc := range map[string]struct {
		intervals []store.TradingPnL
		want      float64
	}{
		// A $500 buy stored after the snapshot its cash left before: a
		// withdrawal and a $500 gain, reversed next interval.
		"late fill":            {pnl([2]string{"0", "0"}, [2]string{"500", "-500"}, [2]string{"-500", "500"}), 0},
		"late fill, then loss": {pnl([2]string{"500", "-500"}, [2]string{"-500", "500"}, [2]string{"-30", "0"}), 30},
		"gain kept":            {pnl([2]string{"100", "-500"}, [2]string{"0", "0"}, [2]string{"-40", "0"}), 40},
		"latest flow":          {pnl([2]string{"100", "0"}, [2]string{"400", "-500"}), 0},
		"loss after a peak":    {pnl([2]string{"100", "0"}, [2]string{"-60", "0"}, [2]string{"20", "0"}), 40},
		"loss from the start":  {pnl([2]string{"-25", "0"}), 25},
	} {
		got, err := DollarDrawdown(tc.intervals)
		if err != nil || got != tc.want {
			t.Errorf("%s: %v %v, want %v", name, got, err, tc.want)
		}
	}
	if _, err := DollarDrawdown(pnl([2]string{"NaN", "0"})); err == nil {
		t.Error("NaN pnl accepted")
	}
}
