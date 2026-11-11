package main

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
	"github.com/Cyvadra/polymarket-clob-client/pkg/settlement"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// historyStore is where the observer keeps what a maintainer queries later.
type historyStore interface {
	StartDaemonRun(ctx context.Context, run store.DaemonRun) (int64, error)
	StopDaemonRun(ctx context.Context, runID int64, at time.Time) error
	RecordSweep(ctx context.Context, sweep store.SweepRecord) error
	RecordRiskEvent(ctx context.Context, event store.RiskEvent) error
}

// observer sends each operational fact to /metrics and, when it is history
// worth querying later, to the database. A failed write is logged and counted,
// never returned: observability must not stop trading.
type observer struct {
	*metrics
	history historyStore
	now     func() time.Time

	mu        sync.Mutex
	runID     int64
	pending   *store.DaemonRun // a run whose record failed, retried on the next write
	suspended map[string]bool  // guard label -> last seen state
}

func newObserver(m *metrics, history historyStore, now func() time.Time) *observer {
	return &observer{metrics: m, history: history, now: now, suspended: map[string]bool{}}
}

// startRun records this process and its non-secret settings.
func (o *observer) startRun(ctx context.Context, signer, maker string, cfg config) {
	o.mu.Lock()
	o.pending = &store.DaemonRun{
		Version: buildVersion(), Signer: signer, Maker: maker,
		Strategies: cfg.AllowedStrategies.Names(), Config: runConfig(cfg),
	}
	o.mu.Unlock()
	o.ensureRun(ctx)
}

// ensureRun records the pending run, if any, and returns this process's run
// ID: 0 while it is not recorded. A failed record is retried on the next
// history write, so a brief database outage at startup does not leave the
// whole run's rows unlinked.
func (o *observer) ensureRun(ctx context.Context) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.runID != 0 || o.pending == nil {
		return o.runID
	}
	id, err := o.history.StartDaemonRun(ctx, *o.pending)
	if err != nil {
		o.historyFailed(err)
		return 0
	}
	o.runID, o.pending = id, nil
	return id
}

// stopRun stamps this process's stop time, with its own deadline since the
// run context is done by then.
func (o *observer) stopRun() {
	runID := o.run()
	if runID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := o.history.StopDaemonRun(ctx, runID, o.now()); err != nil {
		o.historyFailed(err)
	}
}

func (o *observer) run() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.runID
}

// sweep records a pass on /metrics, and in the database when it changed a
// lane or failed, so quiet passes do not flood the table. Lanes left closing
// or settling are a standing state, counted again every pass, not a change.
func (o *observer) sweep(ctx context.Context, s settlement.Sweep) {
	at := o.now()
	o.observeSweep(s, at)
	if s.Lost+s.Redeemed+s.Shrunk+s.Dust == 0 && !s.Failed {
		return
	}
	if err := o.history.RecordSweep(ctx, store.SweepRecord{
		RunID: o.ensureRun(ctx), At: at, Checked: s.Checked, Lost: s.Lost, Redeemed: s.Redeemed,
		Dust: s.Dust, Shrunk: s.Shrunk, Closing: s.Reserved, Settling: s.Settling, Failed: s.Failed,
	}); err != nil {
		o.historyFailed(err)
	}
}

// guard records a guard's reading, and a risk event when it starts or stops
// holding back opens. The first reading of a guard that is not suspended is
// not an event.
func (o *observer) guard(ctx context.Context, guard *equity.DrawdownGuard) {
	o.observeGuard(guard)
	suspended, reason := guard.Suspended()
	label := guardLabel(guard)
	o.mu.Lock()
	was, seen := o.suspended[label]
	o.suspended[label] = suspended
	o.mu.Unlock()
	if suspended == was && (seen || !suspended) {
		return
	}
	kind := store.RiskResume
	if suspended {
		kind = store.RiskSuspend
	} else {
		reason = guard.Label() + " " + guard.Status()
	}
	drawdown, limit, dollars := guard.Reading()
	o.riskEvent(ctx, kind, label, reason, map[string]any{"drawdown": drawdown, "limit": limit, "dollars": dollars})
}

func (o *observer) sizingBase(ctx context.Context, base equity.Anchor) {
	o.observeSizingBase(base.EquityUSD)
	o.riskEvent(ctx, store.RiskSizingBase, "", base.Snapshot.Reason, map[string]any{
		"equity_usd": base.EquityUSD, "snapshot_equity_usd": base.Snapshot.EquityUSD,
		"snapshot_taken_at": base.Snapshot.TakenAt, "external_flow_usd": base.FlowUSD,
	})
}

func (o *observer) equityRecorded(ctx context.Context, reason string, snapshot store.EquitySnapshotRecord) {
	o.observeEquity(snapshot.EquityUSD, o.now())
	o.riskEvent(ctx, store.RiskEquityRecorded, "", reason, map[string]any{
		"equity_usd": snapshot.EquityUSD, "trade_cash_usd": snapshot.TradeCashUSD,
		"external_flow_usd": snapshot.ExternalFlowUSD, "trade_index": snapshot.TradeIndex,
	})
}

func (o *observer) snapshotDeferred(ctx context.Context, winnersHeld int) {
	o.riskEvent(ctx, store.RiskSnapshotDeferred, "", "winning lanes not yet settled", map[string]any{"winners_held": winnersHeld})
}

func (o *observer) riskEvent(ctx context.Context, kind, subject, reason string, details map[string]any) {
	if err := o.history.RecordRiskEvent(ctx, store.RiskEvent{
		RunID: o.ensureRun(ctx), At: o.now(), Kind: kind, Subject: subject, Reason: reason, Details: details,
	}); err != nil {
		o.historyFailed(err)
	}
}

func (o *observer) historyFailed(err error) {
	log.Printf("observability history: %v", err)
	o.countError("history")
}

func guardLabel(guard *equity.DrawdownGuard) string {
	if guard.Strategy() == "" {
		return "wallet"
	}
	return guard.Strategy()
}

// buildVersion is the VCS revision the binary was built from, if known.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return info.Main.Version
}

// runConfig is the settings a run is recorded with. It names fields one by
// one, so a secret added to config is never recorded by accident.
func runConfig(cfg config) map[string]any {
	limits := map[string]string{}
	for prefix, limit := range cfg.StrategyMaxDrawdown {
		limits[prefix] = fmt.Sprint(limit)
	}
	return map[string]any{
		"max_open_buy_notional_usd":     cfg.MaxOpenBuyNotionalUSD,
		"max_equity_fraction":           cfg.MaxEquityFraction,
		"max_ladder_levels":             cfg.MaxLadderLevels,
		"max_drawdown":                  fmt.Sprint(cfg.MaxDrawdown),
		"strategy_max_drawdown":         limits,
		"strategy_default_max_drawdown": fmt.Sprint(cfg.StrategyDefaultMaxDrawdown),
		"drawdown_start":                cfg.DrawdownStart,
		"size_multipliers":              cfg.SizeMultipliers,
		"reconcile_interval":            cfg.ReconcileInterval.String(),
		"settlement_interval":           cfg.SettlementInterval.String(),
		"feature_interval":              cfg.FeatureInterval.String(),
		"keep_warm_interval":            cfg.KeepWarmInterval.String(),
		"metrics_addr":                  cfg.MetricsAddr,
	}
}
