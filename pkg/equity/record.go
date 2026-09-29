package equity

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	snapshot, err := r.wallet.Snapshot(ctx)
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
	if ok {
		step, err := r.step(prev, snapshot, pending.USD)
		if err != nil {
			return store.EquitySnapshotRecord{}, err
		}
		record.TradeCashUSD = formatUSD(step.tradeCash)
		record.ExternalFlowUSD = formatUSD(step.flow)
		record.TradeIndex = strconv.FormatFloat(step.index, 'f', 12, 64)
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

type step struct{ tradeCash, flow, index float64 }

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
	if base := prevEquity + flow; base > 0 {
		index *= 1 + (now.EquityUSD-prevEquity-flow)/base
	}
	return step{tradeCash: tradeCash, flow: flow, index: max(index, 0)}, nil
}

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
type DrawdownGuard struct {
	store store.EquityStore
	start time.Time
	limit float64

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

// Refresh reads the drawdown from the latest snapshot. With no snapshot since
// the start there is nothing to measure, and nothing is suspended.
func (g *DrawdownGuard) Refresh(ctx context.Context) (float64, error) {
	drawdown, err := g.read(ctx)
	if err != nil {
		return 0, err
	}
	g.mu.Lock()
	g.drawdown = drawdown
	g.mu.Unlock()
	return drawdown, nil
}

func (g *DrawdownGuard) read(ctx context.Context) (float64, error) {
	latest, ok, err := g.store.LatestEquitySnapshot(ctx)
	if err != nil || !ok || latest.TakenAt.Before(g.start) {
		return 0, err
	}
	peakText, ok, err := g.store.PeakTradeIndexSince(ctx, g.start)
	if err != nil || !ok {
		return 0, err
	}
	peak, err := decimal.NonNegativeFloat(peakText)
	if err != nil {
		return 0, fmt.Errorf("peak trade index: %w", err)
	}
	index, err := decimal.NonNegativeFloat(latest.TradeIndex)
	if err != nil {
		return 0, fmt.Errorf("trade index: %w", err)
	}
	return Drawdown(peak, index), nil
}

// Suspended reports whether new positions are held back, and why.
func (g *DrawdownGuard) Suspended() (bool, string) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.drawdown <= g.limit {
		return false, ""
	}
	return true, fmt.Sprintf("trading drawdown %.2f%% since %s exceeds the %.2f%% limit",
		g.drawdown*100, g.start.Format(time.RFC3339), g.limit*100)
}
