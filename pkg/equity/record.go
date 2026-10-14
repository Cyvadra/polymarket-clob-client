package equity

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// ErrIncompleteValuation is returned instead of recording a snapshot in which
// some position could not be looked up: its value is a guess, and a guess
// recorded low would read as a trading loss.
var ErrIncompleteValuation = errors.New("equity valuation incomplete")

// DefaultFlowThresholdUSD is the smallest unexplained cash change taken as a
// deposit or withdrawal. Anything smaller (rounding, a fee the fill did not
// carry, a rebate) stays in the trading return.
const DefaultFlowThresholdUSD = 1.0

type valuer interface {
	Invalidate()
	Snapshot(context.Context) (Snapshot, error)
	SnapshotWithStrategies(context.Context) (Snapshot, error)
}

// Recorder saves the wallet's equity at moments worth keeping, such as after
// positions settle. The history has gaps by design. Each row separates what
// trading earned from deposits and withdrawals and chains the former into a
// trade index, which a drawdown limit is measured on.
type Recorder struct {
	wallet        valuer
	store         store.EquityStore
	flowThreshold float64
}

// NewRecorder takes cash changes smaller than flowThresholdUSD as trading;
// see DefaultFlowThresholdUSD.
func NewRecorder(wallet *Tracker, s store.EquityStore, flowThresholdUSD float64) (*Recorder, error) {
	if wallet == nil || s == nil {
		return nil, fmt.Errorf("equity tracker and equity store are required")
	}
	return &Recorder{wallet: wallet, store: s, flowThreshold: flowThresholdUSD}, nil
}

// Record values the wallet afresh and saves it under reason.
func (r *Recorder) Record(ctx context.Context, reason string) (store.EquitySnapshotRecord, error) {
	// Trade cash is read before the balance, so every fill and payout it
	// counts has already moved the cash read below. One stored meanwhile is
	// left to the next snapshot.
	pending, err := r.store.PendingTradeCash(ctx)
	if err != nil {
		return store.EquitySnapshotRecord{}, err
	}
	// The loss is read from the store rather than the caller, so one swept
	// before a restart still tags the snapshot that counts it.
	if pending.Lost {
		reason = ReasonSettlementLoss
	}
	// Settlement moves cash on chain; a cached balance would miss it.
	r.wallet.Invalidate()
	snapshot, err := r.wallet.SnapshotWithStrategies(ctx)
	if err != nil {
		return store.EquitySnapshotRecord{}, err
	}
	if snapshot.LookupFailures > 0 {
		return store.EquitySnapshotRecord{}, fmt.Errorf("%w: %d positions could not be looked up", ErrIncompleteValuation, snapshot.LookupFailures)
	}
	record := store.EquitySnapshotRecord{
		// Cash is what the flow is worked out from, so the row is dated by
		// when it was read.
		TakenAt:           snapshot.CashAsOf,
		Reason:            reason,
		CashUSD:           formatUSD(snapshot.CashUSD),
		PositionsUSD:      formatUSD(snapshot.PositionsUSD),
		EquityUSD:         formatUSD(snapshot.EquityUSD),
		TradeCashUSD:      formatUSD(0),
		ExternalFlowUSD:   formatUSD(0),
		TradeIndex:        "1",
		UnmarkedPositions: snapshot.UnmarkedPositions,
	}
	prev, ok, err := r.store.LatestEquitySnapshot(ctx)
	if err != nil {
		return store.EquitySnapshotRecord{}, err
	}
	var base float64
	if ok {
		step, err := r.step(prev, snapshot, pending.USD)
		if err != nil {
			return store.EquitySnapshotRecord{}, err
		}
		base = step.base
		record.TradeCashUSD = formatUSD(step.tradeCash)
		record.ExternalFlowUSD = formatUSD(step.flow)
		record.TradeIndex = formatIndex(step.index)
	}
	record.Strategies, err = r.strategies(ctx, prev, ok, base, snapshot, pending.ByStrategy)
	if err != nil {
		return store.EquitySnapshotRecord{}, err
	}
	return r.store.RecordEquitySnapshot(ctx, record, pending)
}

// CashMoved reports whether the wallet's cash has moved by a deposit or
// withdrawal since the latest snapshot. Snapshots otherwise follow settlements
// only, and a wallet that is not trading would never record the transfer.
func (r *Recorder) CashMoved(ctx context.Context) (bool, error) {
	prev, ok, err := r.store.LatestEquitySnapshot(ctx)
	if err != nil || !ok {
		return false, err
	}
	pending, err := r.store.PendingTradeCash(ctx)
	if err != nil {
		return false, err
	}
	snapshot, err := r.wallet.Snapshot(ctx)
	if err != nil {
		return false, err
	}
	step, err := r.step(prev, snapshot, pending.USD)
	if err != nil {
		return false, err
	}
	return step.flow != 0, nil
}

type step struct{ tradeCash, flow, base, index float64 }

// step works out what happened since prev, given the trade cash no snapshot
// has counted yet. Cash moves by trading (fills and payouts, which the store
// recorded) and by deposits and withdrawals (which it
// did not); the rest is the flow. The trading return is the equity change less
// the flow, over the equity it was earned on, with the flow taken to land at
// the start of the interval.
func (r *Recorder) step(prev store.EquitySnapshotRecord, now Snapshot, tradeCashText string) (step, error) {
	prevCash, err := decimal.NonNegativeFloat(prev.CashUSD)
	if err != nil {
		return step{}, fmt.Errorf("previous snapshot %d cash: %w", prev.ID, err)
	}
	prevEquity, err := decimal.NonNegativeFloat(prev.EquityUSD)
	if err != nil {
		return step{}, fmt.Errorf("previous snapshot %d equity: %w", prev.ID, err)
	}
	prevIndex, err := decimal.NonNegativeFloat(prev.TradeIndex)
	if err != nil {
		return step{}, fmt.Errorf("previous snapshot %d trade index: %w", prev.ID, err)
	}
	tradeCash, err := strconv.ParseFloat(tradeCashText, 64)
	if err != nil {
		return step{}, fmt.Errorf("trade cash %q: %w", tradeCashText, err)
	}
	flow := (now.CashUSD - prevCash) - tradeCash
	if math.Abs(flow) < r.flowThreshold {
		flow = 0
	}
	index := prevIndex
	base := prevEquity + flow
	if base > 0 {
		index *= 1 + (now.EquityUSD-prevEquity-flow)/base
	}
	return step{tradeCash: tradeCash, flow: flow, base: base, index: max(index, 0)}, nil
}

// strategies splits the snapshot's trading by strategy. A strategy's return
// over the interval is what its lanes made, their trade cash plus the change
// in what they hold, over the same base as the wallet's return, so the
// strategies' returns in an interval add up to the wallet's. Lanes with no
// recorded strategy are kept together under "", so the split is always whole
// and a snapshot with positions but no rows can only predate the split. A strategy seen for the first time starts at 1. When the
// previous snapshot has no split and the wallet held positions then, what each
// strategy held is unknown, so the interval is left out of every index.
func (r *Recorder) strategies(ctx context.Context, prev store.EquitySnapshotRecord, hasPrev bool, base float64, now Snapshot, tradeCash map[string]string) ([]store.StrategyEquityRecord, error) {
	type part struct{ positions, index float64 }
	previous := map[string]part{}
	if hasPrev {
		rows, err := r.store.StrategyEquity(ctx, prev.ID)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			positions, err := decimal.NonNegativeFloat(row.PositionsUSD)
			if err != nil {
				return nil, fmt.Errorf("previous snapshot %d strategy %q positions: %w", prev.ID, row.Strategy, err)
			}
			index, err := decimal.NonNegativeFloat(row.TradeIndex)
			if err != nil {
				return nil, fmt.Errorf("previous snapshot %d strategy %q trade index: %w", prev.ID, row.Strategy, err)
			}
			previous[row.Strategy] = part{positions: positions, index: index}
		}
		// With no split recorded, what the strategies held then is known
		// only if the wallet held nothing; otherwise skip the interval.
		if len(rows) == 0 {
			held, err := decimal.NonNegativeFloat(prev.PositionsUSD)
			if err != nil {
				return nil, fmt.Errorf("previous snapshot %d positions: %w", prev.ID, err)
			}
			if held > 0 {
				base = 0
			}
		}
	} else {
		base = 0
	}
	names := map[string]struct{}{}
	for name := range previous {
		names[name] = struct{}{}
	}
	for name := range now.PositionsByStrategy {
		names[name] = struct{}{}
	}
	for name := range tradeCash {
		names[name] = struct{}{}
	}
	parts := make([]store.StrategyEquityRecord, 0, len(names))
	for name := range names {
		cash := 0.0
		if text, ok := tradeCash[name]; ok {
			value, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, fmt.Errorf("strategy %q trade cash %q: %w", name, text, err)
			}
			cash = value
		}
		positions := now.PositionsByStrategy[name]
		index := 1.0
		if prior, ok := previous[name]; ok {
			index = prior.index
			if base > 0 {
				index *= 1 + (cash+positions-prior.positions)/base
			}
		} else if base > 0 {
			// New since the previous snapshot: it held nothing then.
			index *= 1 + (cash+positions)/base
		}
		parts = append(parts, store.StrategyEquityRecord{
			Strategy:     name,
			TradeCashUSD: formatIndex(cash),
			PositionsUSD: formatIndex(positions),
			TradeIndex:   formatIndex(max(index, 0)),
		})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Strategy < parts[j].Strategy })
	return parts, nil
}

func formatIndex(v float64) string { return strconv.FormatFloat(v, 'f', 12, 64) }

func formatUSD(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }

// Drawdown is how far value sits below peak, as a fraction of peak. It is 0
// at or above the peak and when there is no positive peak to measure from.
func Drawdown(peak, value float64) float64 {
	if peak <= 0 || value >= peak {
		return 0
	}
	return (peak - value) / peak
}

// DrawdownLimit is how far trading may fall from its peak: a fraction of the
// peak index, or a dollar amount. Make one with FractionLimit or DollarLimit;
// the zero value is no limit.
//
// A dollar drawdown is the fall of cumulative trading profit and loss, in
// dollars, from its highest value since the start. Each interval's dollars
// are its equity change less the flow (a strategy's: its trade cash plus the
// change in what it holds), so deposits and withdrawals do not move it.
type DrawdownLimit struct {
	dollars bool
	value   float64
}

// FractionLimit is a limit of fraction of the peak trade index, in (0, 1).
func FractionLimit(fraction float64) DrawdownLimit { return DrawdownLimit{value: fraction} }

// DollarLimit is a limit of usd dollars below the peak trading profit.
func DollarLimit(usd float64) DrawdownLimit { return DrawdownLimit{dollars: true, value: usd} }

// Valid reports whether the limit is set to a finite value in range: a
// fraction in (0, 1) or a positive number of dollars.
func (l DrawdownLimit) Valid() bool {
	if l.dollars {
		return l.value > 0 && !math.IsInf(l.value, 0)
	}
	return l.value > 0 && l.value < 1
}

// Dollars reports whether the limit is a dollar amount.
func (l DrawdownLimit) Dollars() bool { return l.dollars }

// IsZero reports whether no limit is set.
func (l DrawdownLimit) IsZero() bool { return l == DrawdownLimit{} }

// String formats the limit as it is configured: "$123" or "20%".
func (l DrawdownLimit) String() string {
	if l.dollars {
		return "$" + strconv.FormatFloat(l.value, 'f', -1, 64)
	}
	// Rounding to twelve digits keeps 0.07*100 from printing as
	// 7.000000000000001; 'f' keeps a small one out of exponent form.
	percent, _ := strconv.ParseFloat(strconv.FormatFloat(l.value*100, 'g', 12, 64), 64)
	return strconv.FormatFloat(percent, 'f', -1, 64) + "%"
}

// DrawdownGuard suspends new positions while trading sits more than a set
// limit below its peak since a start the operator chose: the trade index for
// a fraction limit, cumulative trading dollars for a dollar limit. The peak
// includes the level trading stood at when the start came, so a loss across
// the start is measured. It reads
// the recorded snapshots, so it moves only when one is recorded; Refresh
// after each. Recovery needs trading to climb back, or a later start.
//
// A guard made by NewStrategyDrawdownGuard measures one strategy's trading
// instead, and holds back only that strategy.
type DrawdownGuard struct {
	store    store.EquityStore
	start    time.Time
	limit    DrawdownLimit
	strategy string

	mu       sync.RWMutex
	drawdown float64 // a fraction of the peak, or dollars, as the limit is
}

func NewDrawdownGuard(s store.EquityStore, start time.Time, limit DrawdownLimit) (*DrawdownGuard, error) {
	if s == nil {
		return nil, fmt.Errorf("equity store is required")
	}
	if !limit.Valid() {
		return nil, fmt.Errorf("drawdown limit must be a fraction in (0, 1) or a positive dollar amount")
	}
	return &DrawdownGuard{store: s, start: start, limit: limit}, nil
}

// NewStrategyDrawdownGuard measures strategy's trading; see
// store.StrategyEquityRecord.
func NewStrategyDrawdownGuard(s store.EquityStore, strategy string, start time.Time, limit DrawdownLimit) (*DrawdownGuard, error) {
	if strategy == "" {
		return nil, fmt.Errorf("strategy is required")
	}
	guard, err := NewDrawdownGuard(s, start, limit)
	if err != nil {
		return nil, err
	}
	guard.strategy = strategy
	return guard, nil
}

// Strategy is the strategy the guard measures, empty for the whole wallet.
func (g *DrawdownGuard) Strategy() string { return g.strategy }

// Label names what the guard limits, for logs and suspension reasons.
func (g *DrawdownGuard) Label() string {
	if g.strategy == "" {
		return "trading drawdown"
	}
	return fmt.Sprintf("strategy %q drawdown", g.strategy)
}

// Refresh reads the drawdown from the latest snapshot, in the limit's unit: a
// fraction of the peak, or dollars. With no snapshot since the start there is
// nothing to measure, and nothing is suspended.
func (g *DrawdownGuard) Refresh(ctx context.Context) (float64, error) {
	latest, ok, err := g.store.LatestEquitySnapshot(ctx)
	if err != nil {
		return 0, err
	}
	return g.refresh(ctx, latest, ok, nil)
}

// refresh is Refresh given the latest snapshot, and for a strategy guard its
// split when the caller has it already.
func (g *DrawdownGuard) refresh(ctx context.Context, latest store.EquitySnapshotRecord, ok bool, parts []store.StrategyEquityRecord) (float64, error) {
	read := g.readFraction
	if g.limit.Dollars() {
		read = g.readDollars
	}
	drawdown := 0.0
	if ok && !latest.TakenAt.Before(g.start) {
		var err error
		if drawdown, err = read(ctx, latest, parts); err != nil {
			return 0, err
		}
	}
	g.mu.Lock()
	g.drawdown = drawdown
	g.mu.Unlock()
	return drawdown, nil
}

func (g *DrawdownGuard) readFraction(ctx context.Context, latest store.EquitySnapshotRecord, parts []store.StrategyEquityRecord) (float64, error) {
	var err error
	ok := true
	indexText, peakText := latest.TradeIndex, ""
	if g.strategy == "" {
		peakText, ok, err = g.store.PeakTradeIndexSince(ctx, g.start)
	} else {
		if parts == nil {
			if parts, err = g.store.StrategyEquity(ctx, latest.ID); err != nil {
				return 0, err
			}
		}
		indexText, ok = indexIn(parts, g.strategy)
		if !ok {
			return 0, nil
		}
		peakText, ok, err = g.store.PeakStrategyIndexSince(ctx, g.strategy, g.start)
	}
	if err != nil || !ok {
		return 0, err
	}
	peak, err := decimal.NonNegativeFloat(peakText)
	if err != nil {
		return 0, fmt.Errorf("peak trade index: %w", err)
	}
	index, err := decimal.NonNegativeFloat(indexText)
	if err != nil {
		return 0, fmt.Errorf("trade index: %w", err)
	}
	return Drawdown(peak, index), nil
}

func (g *DrawdownGuard) readDollars(ctx context.Context, latest store.EquitySnapshotRecord, _ []store.StrategyEquityRecord) (float64, error) {
	intervals, ok, err := g.store.TradingPnLSince(ctx, g.strategy, g.start, latest.ID)
	if err != nil || !ok {
		return 0, err
	}
	return DollarDrawdown(intervals)
}

// DollarDrawdown is how far the running total of intervals' dollars ends
// below its highest value, counting the level before the first as zero.
//
// A fill whose cash left the wallet before a snapshot but was stored after it
// reads as a withdrawal and a matching gain in that interval, reversed in the
// next. So the total after an interval with an external flow counts toward
// the peak only as far as the next interval keeps it, and a transient spike
// never becomes a lasting peak. A real gain made alongside a transfer and
// lost in the very next interval goes uncounted as a drawdown; the earlier
// peak still counts.
func DollarDrawdown(intervals []store.TradingPnL) (float64, error) {
	var total, peak float64
	pending, held := 0.0, false
	for _, interval := range intervals {
		pnl, err := strconv.ParseFloat(interval.PnLUSD, 64)
		if err != nil || math.IsNaN(pnl) || math.IsInf(pnl, 0) {
			return 0, fmt.Errorf("trading pnl %q at snapshot %d is not a number", interval.PnLUSD, interval.SnapshotID)
		}
		flow, err := strconv.ParseFloat(interval.ExternalFlowUSD, 64)
		if err != nil {
			return 0, fmt.Errorf("external flow %q at snapshot %d is not a number", interval.ExternalFlowUSD, interval.SnapshotID)
		}
		total += pnl
		if held {
			peak = max(peak, min(pending, total))
		}
		pending, held = total, flow != 0
		if !held {
			peak = max(peak, total)
		}
	}
	return max(peak-total, 0), nil
}

// indexIn is the strategy's index in parts. It reports false when the
// strategy is not there, having never traded.
func indexIn(parts []store.StrategyEquityRecord, strategy string) (string, bool) {
	for _, part := range parts {
		if part.Strategy == strategy {
			return part.TradeIndex, true
		}
	}
	return "", false
}

// Suspended reports whether new positions are held back, and why.
func (g *DrawdownGuard) Suspended() (bool, string) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.drawdown <= g.limit.value {
		return false, ""
	}
	return true, fmt.Sprintf("%s %s since %s exceeds the %s limit",
		g.Label(), g.measured(), g.start.Format(time.RFC3339), g.limit)
}

// Status is the drawdown as last read, against the limit it is compared to:
// "$30.00 of the $500 limit" or "3.20% of the 20% limit".
func (g *DrawdownGuard) Status() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return fmt.Sprintf("%s of the %s limit", g.measured(), g.limit)
}

// measured formats the drawdown as last read; g.mu must be held.
func (g *DrawdownGuard) measured() string {
	if g.limit.Dollars() {
		return fmt.Sprintf("$%.2f", g.drawdown)
	}
	return fmt.Sprintf("%.2f%%", g.drawdown*100)
}

// DrawdownLimits holds back opens while the wallet's drawdown is over its
// limit, and a strategy's opens while that strategy's is over its own. With a
// default set, a strategy that has no limit of its own gets the default once
// it first appears in a snapshot's split; one that has never traded has no
// drawdown to limit.
type DrawdownLimits struct {
	store store.EquityStore
	start time.Time

	wallet       *DrawdownGuard
	defaultLimit DrawdownLimit
	covers       func(string) bool

	refreshMu  sync.Mutex // serializes Refresh
	mu         sync.RWMutex
	strategies map[string]*DrawdownGuard
	guards     []*DrawdownGuard // the wallet's first, then the strategies' in name order
}

// NewDrawdownLimits takes a nil wallet guard for no wallet limit. s and start
// are what default strategy guards are measured with.
func NewDrawdownLimits(s store.EquityStore, start time.Time, wallet *DrawdownGuard, strategies ...*DrawdownGuard) *DrawdownLimits {
	limits := &DrawdownLimits{store: s, start: start, wallet: wallet, strategies: map[string]*DrawdownGuard{}}
	for _, guard := range strategies {
		limits.strategies[guard.strategy] = guard
	}
	limits.sortGuards()
	return limits
}

// SetStrategyDefault limits every strategy covers accepts that has no guard
// of its own, as it appears. Call it once, before the first Refresh.
func (l *DrawdownLimits) SetStrategyDefault(limit DrawdownLimit, covers func(string) bool) error {
	if l.store == nil {
		return fmt.Errorf("equity store is required")
	}
	if !limit.Valid() {
		return fmt.Errorf("drawdown limit must be a fraction in (0, 1) or a positive dollar amount")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.defaultLimit.IsZero() {
		return fmt.Errorf("strategy default drawdown limit is already set")
	}
	l.defaultLimit, l.covers = limit, covers
	return nil
}

// sortGuards rebuilds guards from wallet and strategies; l.mu must be held
// or l not yet shared.
func (l *DrawdownLimits) sortGuards() {
	guards := make([]*DrawdownGuard, 0, len(l.strategies)+1)
	if l.wallet != nil {
		guards = append(guards, l.wallet)
	}
	sorted := make([]*DrawdownGuard, 0, len(l.strategies))
	for _, guard := range l.strategies {
		sorted = append(sorted, guard)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].strategy < sorted[j].strategy })
	l.guards = append(guards, sorted...)
}

// Guards lists the wallet guard, if any, then the strategies' in name order.
func (l *DrawdownLimits) Guards() []*DrawdownGuard {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.guards
}

// Refresh refreshes every guard from one read of the latest snapshot and its
// split, so one that fails does not leave the others stale. It first adds a
// default guard for each strategy new to the split. It returns the guards
// that refreshed, in Guards order, and the errors joined.
func (l *DrawdownLimits) Refresh(ctx context.Context) ([]*DrawdownGuard, error) {
	l.refreshMu.Lock()
	defer l.refreshMu.Unlock()
	l.mu.RLock()
	defaulted := !l.defaultLimit.IsZero()
	empty := len(l.guards) == 0
	l.mu.RUnlock()
	if empty && !defaulted {
		return nil, nil
	}
	latest, ok, err := l.store.LatestEquitySnapshot(ctx)
	if err != nil {
		return nil, err
	}
	var parts []store.StrategyEquityRecord
	if ok && (defaulted || l.fractionStrategies()) {
		if parts, err = l.store.StrategyEquity(ctx, latest.ID); err != nil {
			return nil, err
		}
		if parts == nil {
			parts = []store.StrategyEquityRecord{}
		}
	}
	var errs []error
	if defaulted {
		if err := l.addDefaults(parts); err != nil {
			errs = append(errs, err)
		}
	}
	var refreshed []*DrawdownGuard
	for _, guard := range l.Guards() {
		if _, err := guard.refresh(ctx, latest, ok, parts); err != nil {
			errs = append(errs, err)
			continue
		}
		refreshed = append(refreshed, guard)
	}
	return refreshed, errors.Join(errs...)
}

// addDefaults gives each covered strategy in parts without a guard the
// default limit, skipping any it cannot build. Unattributed lanes, kept under "", are no strategy's.
func (l *DrawdownLimits) addDefaults(parts []store.StrategyEquityRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	added := false
	var errs []error
	for _, part := range parts {
		name := part.Strategy
		if name == "" || l.strategies[name] != nil || (l.covers != nil && !l.covers(name)) {
			continue
		}
		guard, err := NewStrategyDrawdownGuard(l.store, name, l.start, l.defaultLimit)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		l.strategies[name] = guard
		added = true
	}
	if added {
		l.sortGuards()
	}
	return errors.Join(errs...)
}

// fractionStrategies reports whether a strategy guard reads the split's
// index, which only fraction limits do.
func (l *DrawdownLimits) fractionStrategies() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, guard := range l.strategies {
		if !guard.limit.Dollars() {
			return true
		}
	}
	return false
}

// Suspended reports whether opens for strategy are held back, and why.
func (l *DrawdownLimits) Suspended(strategy string) (bool, string) {
	if l.wallet != nil {
		if suspended, reason := l.wallet.Suspended(); suspended {
			return true, reason
		}
	}
	l.mu.RLock()
	guard, ok := l.strategies[strategy]
	l.mu.RUnlock()
	if ok {
		return guard.Suspended()
	}
	return false, ""
}
