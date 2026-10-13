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

// DrawdownGuard suspends new positions while the trade index sits more than
// a set fraction below its peak since a start the operator chose. The peak
// includes the index trading stood at when the start came, so a loss across
// the start is measured. It reads
// the recorded snapshots, so it moves only when one is recorded; Refresh
// after each. Recovery needs the index to climb back, or a later start.
//
// A guard made by NewStrategyDrawdownGuard measures one strategy's index
// instead, and holds back only that strategy.
type DrawdownGuard struct {
	store    store.EquityStore
	start    time.Time
	limit    float64
	strategy string

	mu       sync.RWMutex
	drawdown float64
}

func NewDrawdownGuard(s store.EquityStore, start time.Time, limit float64) (*DrawdownGuard, error) {
	if s == nil {
		return nil, fmt.Errorf("equity store is required")
	}
	if limit <= 0 || limit >= 1 {
		return nil, fmt.Errorf("drawdown limit must be in (0, 1)")
	}
	return &DrawdownGuard{store: s, start: start, limit: limit}, nil
}

// NewStrategyDrawdownGuard measures strategy's trade index; see
// store.StrategyEquityRecord.
func NewStrategyDrawdownGuard(s store.EquityStore, strategy string, start time.Time, limit float64) (*DrawdownGuard, error) {
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

// Refresh reads the drawdown from the latest snapshot. With no snapshot since
// the start there is nothing to measure, and nothing is suspended.
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
	drawdown, err := g.read(ctx, latest, ok, parts)
	if err != nil {
		return 0, err
	}
	g.mu.Lock()
	g.drawdown = drawdown
	g.mu.Unlock()
	return drawdown, nil
}

func (g *DrawdownGuard) read(ctx context.Context, latest store.EquitySnapshotRecord, ok bool, parts []store.StrategyEquityRecord) (float64, error) {
	if !ok || latest.TakenAt.Before(g.start) {
		return 0, nil
	}
	var err error
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
	if g.drawdown <= g.limit {
		return false, ""
	}
	return true, fmt.Sprintf("%s %.2f%% since %s exceeds the %.2f%% limit",
		g.Label(), g.drawdown*100, g.start.Format(time.RFC3339), g.limit*100)
}

// DrawdownLimits holds back opens while the wallet's drawdown is over its
// limit, and a strategy's opens while that strategy's is over its own.
type DrawdownLimits struct {
	wallet     *DrawdownGuard
	strategies map[string]*DrawdownGuard
	guards     []*DrawdownGuard // the wallet's first, then the strategies' in name order
}

// NewDrawdownLimits takes a nil wallet guard for no wallet limit.
func NewDrawdownLimits(wallet *DrawdownGuard, strategies ...*DrawdownGuard) *DrawdownLimits {
	limits := &DrawdownLimits{wallet: wallet, strategies: map[string]*DrawdownGuard{}}
	if wallet != nil {
		limits.guards = append(limits.guards, wallet)
	}
	sorted := append([]*DrawdownGuard(nil), strategies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].strategy < sorted[j].strategy })
	for _, guard := range sorted {
		limits.strategies[guard.strategy] = guard
	}
	limits.guards = append(limits.guards, sorted...)
	return limits
}

// Guards lists the wallet guard, if any, then the strategies' in name order.
func (l *DrawdownLimits) Guards() []*DrawdownGuard { return l.guards }

// Refresh refreshes every guard from one read of the latest snapshot and its
// split, so one that fails does not leave the others stale. It returns each
// guard's drawdown by strategy ("" for the wallet's) and the errors joined.
func (l *DrawdownLimits) Refresh(ctx context.Context) (map[string]float64, error) {
	guards := l.Guards()
	if len(guards) == 0 {
		return nil, nil
	}
	s := guards[0].store
	drawdowns := make(map[string]float64, len(guards))
	latest, ok, err := s.LatestEquitySnapshot(ctx)
	if err != nil {
		return nil, err
	}
	var parts []store.StrategyEquityRecord
	if ok && len(l.strategies) > 0 {
		if parts, err = s.StrategyEquity(ctx, latest.ID); err != nil {
			return nil, err
		}
		if parts == nil {
			parts = []store.StrategyEquityRecord{}
		}
	}
	var errs []error
	for _, guard := range guards {
		drawdown, err := guard.refresh(ctx, latest, ok, parts)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		drawdowns[guard.strategy] = drawdown
	}
	return drawdowns, errors.Join(errs...)
}

// Suspended reports whether opens for strategy are held back, and why.
func (l *DrawdownLimits) Suspended(strategy string) (bool, string) {
	if l.wallet != nil {
		if suspended, reason := l.wallet.Suspended(); suspended {
			return true, reason
		}
	}
	if guard, ok := l.strategies[strategy]; ok {
		return guard.Suspended()
	}
	return false, ""
}
