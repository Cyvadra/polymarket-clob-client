package equity

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// Reasons an equity snapshot is recorded under.
const (
	// ReasonSettlement is a snapshot after lanes settled, none of them lost.
	ReasonSettlement = "settlement"
	// ReasonSettlementLoss is a snapshot after at least one lane settled as a
	// losing token since the previous snapshot.
	ReasonSettlementLoss = "settlement-loss"
	// ReasonSizingBase is the first base of loss-anchored sizing, recorded
	// when it is enabled before any loss has been.
	ReasonSizingBase = "sizing-base"
)

// ErrNoSizingBase means loss-anchored sizing has no equity to size from yet.
var ErrNoSizingBase = errors.New("no loss-anchored sizing base recorded yet")

type AnchorStore interface {
	LatestEquitySnapshotWithReason(ctx context.Context, reasons ...string) (store.EquitySnapshotRecord, bool, error)
	ExternalFlowAfter(ctx context.Context, id int64) (string, error)
}

// LossAnchor is the equity loss-anchored sizing multiplies against: the
// equity recorded after the latest settled loss, or the sizing base recorded
// before any loss was. Profits between losses do not raise it, so entry size
// stays put through a winning streak and is reset, up or down, only once a
// loss has been confirmed. Deposits and withdrawals recorded since move it by
// their amount. It is read from the store, so it survives a restart; Refresh
// after recording any snapshot.
type LossAnchor struct {
	store AnchorStore

	mu     sync.RWMutex
	anchor Anchor
	ok     bool
}

// Anchor is the snapshot sizing is anchored at, the external flow recorded
// after it, and the equity entries are sized from: their sum, floored at 0.
type Anchor struct {
	Snapshot  store.EquitySnapshotRecord
	FlowUSD   float64
	EquityUSD float64
}

func NewLossAnchor(s AnchorStore) (*LossAnchor, error) {
	if s == nil {
		return nil, fmt.Errorf("equity store is required")
	}
	return &LossAnchor{store: s}, nil
}

// Refresh reads the latest anchor snapshot and the external flow since. It
// reports false when no anchor has been recorded yet.
func (a *LossAnchor) Refresh(ctx context.Context) (Anchor, bool, error) {
	record, ok, err := a.store.LatestEquitySnapshotWithReason(ctx, ReasonSettlementLoss, ReasonSizingBase)
	if err != nil || !ok {
		return Anchor{}, false, err
	}
	equity, err := decimal.NonNegativeFloat(record.EquityUSD)
	if err != nil {
		return Anchor{}, false, fmt.Errorf("anchor snapshot %d equity: %w", record.ID, err)
	}
	flowText, err := a.store.ExternalFlowAfter(ctx, record.ID)
	if err != nil {
		return Anchor{}, false, err
	}
	flow, err := strconv.ParseFloat(flowText, 64)
	if err != nil {
		return Anchor{}, false, fmt.Errorf("external flow since snapshot %d: %w", record.ID, err)
	}
	anchor := Anchor{Snapshot: record, FlowUSD: flow, EquityUSD: max(equity+flow, 0)}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.anchor, a.ok = anchor, true
	return anchor, true, nil
}

// Ready reports whether an anchor has been read.
func (a *LossAnchor) Ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ok
}

// Equity is the anchored equity in dollars.
func (a *LossAnchor) Equity() (float64, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.ok {
		return 0, ErrNoSizingBase
	}
	return a.anchor.EquityUSD, nil
}
