package executiontest

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
)

// Runner drives a single trade through its whole life cycle: baseline, open,
// fill reconciliation, position accounting, an optional hold, close, and a
// final flat check. Each step is a phase that records its own row in the
// report. Phases stop the run only when continuing would be meaningless
// (nothing was opened) or unsafe; accounting mismatches are recorded as
// FAIL but the run still closes the position it opened.
type Runner struct {
	config   Config
	observer *Observer
	report   *Report

	tag   string
	since time.Time

	openShares, openAvg   float64
	closeShares, closeAvg float64
	openSentAt            time.Time
	closeSentAt           time.Time

	// positionSeen records whether the lane's position was ever visible. An
	// absent position after cleanup only proves the close worked if it was.
	positionSeen bool
	// positionClosed is set once the lane is confirmed flat through NATS, so
	// the final safety close is skipped instead of selling shares that are
	// already gone.
	positionClosed bool
}

func NewRunner(config Config, observer *Observer, report *Report) *Runner {
	return &Runner{config: config, observer: observer, report: report}
}

type phase struct {
	name string
	run  func(context.Context) error
}

func (r *Runner) Run(ctx context.Context) error {
	if r.config.Negative {
		if err := r.invalidSchemaCase(ctx); err != nil {
			r.report.Phase("invalid-schema-rejection", StatusFail, "", "", err.Error())
			return err
		}
		r.report.Phase("invalid-schema-rejection", StatusPass, "", "", "executiond published INVALID_INTENT without submitting an order")
	}

	r.tag = fmt.Sprintf("%s-%d", r.config.strategy(), time.Now().UnixNano())
	defer r.finalCleanup(ctx)

	for _, p := range []phase{
		{"baseline-flat", r.baselineFlat},
		{"open", r.open},
		{"open-fills-reconcile", r.openFillsReconcile},
		{"position-open", r.positionOpen},
		{"hold", r.hold},
		{"close", r.close},
		{"close-fills-reconcile", r.closeFillsReconcile},
		{"position-flat", r.positionFlat},
		{"pnl-summary", r.pnlSummary},
		{"no-residual-orders", r.noResidualOrders},
	} {
		select {
		case <-ctx.Done():
			return fmt.Errorf("phase %s: %w", p.name, ctx.Err())
		default:
		}
		if err := p.run(ctx); err != nil {
			return fmt.Errorf("phase %s: %w", p.name, err)
		}
	}
	return nil
}

func (r *Runner) baselineFlat(ctx context.Context) error {
	baseline, err := r.query(ctx, "baseline")
	if err != nil {
		r.report.Phase("baseline-flat", StatusFail, "", "", err.Error())
		return err
	}
	r.report.Note("Initial Position", fmt.Sprintf("%+v", baseline.Positions))
	if hasAssetPosition(baseline, r.config.AssetID) {
		r.report.Phase("baseline-flat", StatusFail, "", "", "existing visible position found; no real BUY was sent")
		return fmt.Errorf("visible position already exists for the supplied filters")
	}
	r.report.Phase("baseline-flat", StatusPass, "", "", "request/reply succeeded with no visible position")
	return nil
}

func (r *Runner) open(ctx context.Context) error {
	request := protocol.ExecutionOpenRequest{
		SchemaVersion: protocol.SchemaVersionV1, UniqueTag: r.tag, Strategy: r.config.strategy(),
		ConditionID: r.config.ConditionID, TokenID: r.config.AssetID, Outcome: r.config.Outcome,
		Side: protocol.SideBuy, TargetUSD: r.config.TargetUSD, LimitPrice: r.config.BuyLimit,
		TimeInForce: protocol.TimeInForceGTC, ExpiresAt: time.Now().UTC().Add(r.config.CaseTimeout),
		Policy: protocol.ExecutionPolicy{Style: protocol.ExecutionStyleLimit, CompleteWithinMillis: r.config.CaseTimeout.Milliseconds()},
	}
	if request.TargetUSD != r.config.TargetUSD {
		return fmt.Errorf("unexpected target amount")
	}
	r.since = time.Now().UTC()
	r.openSentAt = r.since
	r.report.Command(protocol.SubjectStrategyExecutionOpen, r.tag, "BUY", fmt.Sprintf("target_usd=%s limit_price=%s", request.TargetUSD, request.LimitPrice))
	if err := r.observer.Publish(protocol.SubjectStrategyExecutionOpen, request); err != nil {
		r.report.Phase("open", StatusFail, "", "", err.Error())
		return err
	}
	message, err := r.observer.WaitForResult(ctx, protocol.SubjectExecutionOpenResult, r.tag, r.since, r.config.CaseTimeout)
	if err != nil {
		r.report.Phase("open", StatusInconclusive, "", "", err.Error()+"; a GTC BUY whose limit does not cross the ask rests LIVE and has no terminal result")
		return err
	}
	r.report.Evidence(message)
	var result protocol.ExecutionOpenResult
	if err := json.Unmarshal(message.Payload, &result); err != nil {
		return err
	}
	if result.Status != protocol.ResultSucceeded || result.FilledShares <= 0 {
		r.report.Phase("open", StatusFail, fmt.Sprintf("%.8f", result.FilledShares), "", fmt.Sprintf("status=%s reason_code=%s", result.Status, result.ReasonCode))
		return fmt.Errorf("open did not fill: status=%s reason_code=%s", result.Status, result.ReasonCode)
	}
	r.openShares, r.openAvg = result.FilledShares, result.AveragePrice
	r.report.Phase("open", StatusPass, fmt.Sprintf("%.8f", r.openShares), fmt.Sprintf("%.6f", r.openAvg), "OPEN result SUCCEEDED")
	return nil
}

func (r *Runner) openFillsReconcile(ctx context.Context) error {
	r.reconcileFills("open-fills-reconcile", r.openSentAt, r.openShares)
	return nil
}

func (r *Runner) closeFillsReconcile(ctx context.Context) error {
	if r.closeSentAt.IsZero() {
		r.report.Phase("close-fills-reconcile", StatusSkip, "", "", "no close was sent by the close phase")
		return nil
	}
	r.reconcileFills("close-fills-reconcile", r.closeSentAt, r.openShares)
	return nil
}

// reconcileFills sums matched_shares from execution.order.event messages seen
// since the command was sent and compares the total against the shares the
// terminal result reported. Missing events are INCONCLUSIVE (executiond may
// not emit per-fill events on this path); a real mismatch is FAIL, but never
// fatal, so the run still closes the position.
func (r *Runner) reconcileFills(name string, since time.Time, expected float64) {
	events := r.observer.OrderEventsBetween(since, time.Now().UTC())
	if len(events) == 0 {
		r.report.Phase(name, StatusInconclusive, "", "", "no execution.order.event observed for this leg")
		return
	}
	var matched float64
	for _, event := range events {
		matched += event.MatchedShares
	}
	tolerance := math.Max(1e-6, expected*0.01)
	detail := fmt.Sprintf("%d order event(s), Σ matched_shares=%.8f vs result shares=%.8f", len(events), matched, expected)
	if math.Abs(matched-expected) > tolerance {
		r.report.Phase(name, StatusFail, fmt.Sprintf("%.8f", matched), "", detail+" (outside tolerance)")
		return
	}
	r.report.Phase(name, StatusPass, fmt.Sprintf("%.8f", matched), "", detail)
}

func (r *Runner) positionOpen(ctx context.Context) error {
	if err := r.waitPosition(ctx, r.tag, true); err != nil {
		r.report.Phase("position-open", StatusFail, "", "", err.Error())
		return err
	}
	r.positionSeen = true
	response, err := r.query(ctx, "position-open")
	if err != nil {
		r.report.Phase("position-open", StatusInconclusive, "", "", "position visible but re-query failed: "+err.Error())
		return nil
	}
	feature, ok := r.lanePosition(response)
	if !ok {
		r.report.Phase("position-open", StatusInconclusive, "", "", "position cleared between the wait and the accounting read")
		return nil
	}
	drift := math.Abs(feature.PositionSize - r.openShares)
	entry := "nil"
	if feature.EntryPrice != nil {
		entry = fmt.Sprintf("%.6f", *feature.EntryPrice)
	}
	detail := fmt.Sprintf("position_size=%.8f available=%.8f reserved=%.8f entry_price=%s (open filled %.8f @ %.6f)",
		feature.PositionSize, feature.AvailableSize, feature.ReservedSize, entry, r.openShares, r.openAvg)
	if drift > math.Max(1e-6, r.openShares*0.02) {
		r.report.Phase("position-open", StatusFail, fmt.Sprintf("%.8f", feature.PositionSize), entry, detail+" (position_size disagrees with filled shares)")
		return nil
	}
	if feature.ReservedSize > 1e-6 {
		r.report.Phase("position-open", StatusInconclusive, fmt.Sprintf("%.8f", feature.PositionSize), entry, detail+" (unexpected reserved size on a fresh open)")
		return nil
	}
	r.report.Phase("position-open", StatusPass, fmt.Sprintf("%.8f", feature.PositionSize), entry, detail)
	return nil
}

func (r *Runner) hold(ctx context.Context) error {
	if r.config.Hold <= 0 {
		r.report.Phase("hold", StatusSkip, "", "", "no --hold supplied")
		return nil
	}
	before, err := r.query(ctx, "hold-before")
	if err != nil {
		r.report.Phase("hold", StatusInconclusive, "", "", "pre-hold query failed: "+err.Error())
		return nil
	}
	start, _ := r.lanePosition(before)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(r.config.Hold):
	}
	after, err := r.query(ctx, "hold-after")
	if err != nil {
		r.report.Phase("hold", StatusInconclusive, "", "", "post-hold query failed: "+err.Error())
		return nil
	}
	end, ok := r.lanePosition(after)
	if !ok || !end.HasPosition {
		r.report.Phase("hold", StatusFail, "", "", fmt.Sprintf("position disappeared during a %s hold", r.config.Hold))
		return fmt.Errorf("position lost during hold")
	}
	detail := fmt.Sprintf("held %s; position_size %.8f -> %.8f, seconds_since_entry %.1f -> %.1f",
		r.config.Hold, start.PositionSize, end.PositionSize, start.SecondsSinceEntry, end.SecondsSinceEntry)
	if end.SecondsSinceEntry <= start.SecondsSinceEntry {
		r.report.Phase("hold", StatusInconclusive, fmt.Sprintf("%.8f", end.PositionSize), "", detail+" (seconds_since_entry did not advance)")
		return nil
	}
	r.report.Phase("hold", StatusPass, fmt.Sprintf("%.8f", end.PositionSize), "", detail)
	return nil
}

// close exits the lane. With a --sell-limit it sends a LIMIT_CLOSE; mode
// "auto" (the default) falls back to FORCE_CLOSE when the limit does not
// cross, mode "force" always force-closes, mode "limit" never falls back.
func (r *Runner) close(ctx context.Context) error {
	limit := strings.TrimSpace(r.config.SellLimit)
	useLimit := limit != "" && r.config.CloseMode != "force"
	if !useLimit {
		return r.closeOnce(ctx, protocol.ExecutionCloseModeForce, "")
	}
	if err := r.closeOnce(ctx, protocol.ExecutionCloseModeLimit, limit); err != nil {
		if r.config.CloseMode != "auto" {
			return nil
		}
		r.report.Phase("close", StatusInconclusive, "", "", "limit close did not fill; falling back to FORCE_CLOSE")
		return r.closeOnce(ctx, protocol.ExecutionCloseModeForce, "")
	}
	return nil
}

// closeOnce sends one close and records the phase row. It returns an error
// only to signal the auto fallback; the run itself is never stopped by a
// close that did not confirm, because finalCleanup is the backstop.
func (r *Runner) closeOnce(ctx context.Context, mode protocol.ExecutionCloseMode, limit string) error {
	result, err := r.sendClose(ctx, mode, limit)
	if err != nil {
		r.report.Phase("close", StatusInconclusive, "", "", fmt.Sprintf("%s: %v", mode, err))
		return err
	}
	if result.Status != protocol.ResultSucceeded || result.FilledShares <= 0 {
		r.report.Phase("close", StatusFail, fmt.Sprintf("%.8f", result.FilledShares), "", fmt.Sprintf("%s status=%s reason_code=%s", mode, result.Status, result.ReasonCode))
		return fmt.Errorf("%s did not fill", mode)
	}
	r.closeShares, r.closeAvg = result.FilledShares, result.AveragePrice
	r.report.Phase("close", StatusPass, fmt.Sprintf("%.8f", r.closeShares), fmt.Sprintf("%.6f", r.closeAvg), fmt.Sprintf("%s result SUCCEEDED", mode))
	return nil
}

func (r *Runner) positionFlat(ctx context.Context) error {
	if err := r.waitPosition(ctx, r.tag, false); err != nil {
		r.report.Phase("position-flat", StatusInconclusive, "", "", err.Error()+"; final cleanup will attempt a force close")
		return nil
	}
	r.positionClosed = true
	r.report.Phase("position-flat", StatusPass, "0", "", "lane no longer visible through NATS")
	return nil
}

func (r *Runner) pnlSummary(ctx context.Context) error {
	if r.openShares <= 0 || r.closeShares <= 0 {
		r.report.Phase("pnl-summary", StatusSkip, "", "", "need a confirmed open and close to compute P&L")
		return nil
	}
	notionalIn := r.openShares * r.openAvg
	notionalOut := r.closeShares * r.closeAvg
	gross := notionalOut - notionalIn
	perShare := r.closeAvg - r.openAvg
	r.report.Note("P&L Summary", fmt.Sprintf(
		"| Leg | Shares | Avg price | Notional |\n|---|---|---|---|\n| open (buy) | %.8f | %.6f | %.6f |\n| close (sell) | %.8f | %.6f | %.6f |\n\nGross P&L: `%.6f` USD  (%.6f USD/share)",
		r.openShares, r.openAvg, notionalIn, r.closeShares, r.closeAvg, notionalOut, gross, perShare))
	r.report.Phase("pnl-summary", StatusInfo, fmt.Sprintf("%.8f", r.closeShares), fmt.Sprintf("%.6f", perShare), fmt.Sprintf("gross %.6f USD (in %.6f, out %.6f)", gross, notionalIn, notionalOut))
	return nil
}

func (r *Runner) noResidualOrders(ctx context.Context) error {
	events := r.observer.OrderEventsBetween(r.openSentAt, time.Now().UTC())
	if len(events) == 0 {
		r.report.Phase("no-residual-orders", StatusSkip, "", "", "no execution.order.event observed to inspect")
		return nil
	}
	last := events[len(events)-1]
	state := strings.ToUpper(strings.TrimSpace(last.State))
	if state == "LIVE" || state == "OPEN" || state == "PARTIALLY_FILLED" || state == "PENDING" {
		r.report.Phase("no-residual-orders", StatusFail, "", "", fmt.Sprintf("last order event still non-terminal: state=%s intent_id=%s", last.State, last.IntentID))
		return nil
	}
	r.report.Phase("no-residual-orders", StatusPass, "", "", fmt.Sprintf("last order event terminal: state=%s", last.State))
	return nil
}

// finalCleanup runs as a deferred backstop. When the lane is already
// confirmed flat it sends nothing, so the test never sells shares it does
// not have; otherwise it force-closes with its own deadline because the
// open may still be resting after an interrupt.
func (r *Runner) finalCleanup(ctx context.Context) {
	if r.tag == "" {
		return
	}
	if r.positionClosed {
		r.report.Phase("cleanup", StatusSkip, "", "", "lane already confirmed flat; no safety close sent")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.config.CleanupTimeout)
	defer cancel()
	if !r.positionSeen {
		if response, err := r.query(ctx, "pre-cleanup"); err == nil && r.hasLanePosition(response, r.tag) {
			r.positionSeen = true
		}
	}
	if _, err := r.sendClose(ctx, protocol.ExecutionCloseModeForce, ""); err != nil {
		r.report.Phase("cleanup", StatusInconclusive, "", "", "force close send/await failed: "+err.Error())
		return
	}
	if !r.positionSeen {
		r.report.Phase("cleanup", StatusInconclusive, "", "", "close sent, but no position was ever visible for this lane, so an empty position proves nothing")
		return
	}
	if err := r.waitPosition(ctx, r.tag, false); err != nil {
		r.report.Phase("cleanup", StatusInconclusive, "", "", err.Error())
		return
	}
	r.positionClosed = true
	r.report.Phase("cleanup", StatusPass, "0", "", "position no longer visible through NATS")
}

// sendClose publishes one close command and waits for its terminal result.
// It records the command and the evidence but leaves PASS/FAIL to the caller.
func (r *Runner) sendClose(ctx context.Context, mode protocol.ExecutionCloseMode, limit string) (protocol.ExecutionCloseResult, error) {
	request := protocol.ExecutionCloseRequest{
		SchemaVersion: protocol.SchemaVersionV1, UniqueTag: r.tag, Strategy: r.config.strategy(),
		ConditionID: r.config.ConditionID, AssetID: r.config.AssetID, Outcome: r.config.Outcome,
		Mode: mode, CreatedAt: time.Now().UTC(),
	}
	detail := "SELL 0.01 FAK is implemented by executiond"
	if mode == protocol.ExecutionCloseModeLimit {
		request.LimitPrice = limit
		request.TimeInForce = protocol.TimeInForceGTC
		request.Policy = protocol.ExecutionPolicy{Style: protocol.ExecutionStyleLimit, CompleteWithinMillis: r.config.CaseTimeout.Milliseconds()}
		detail = fmt.Sprintf("SELL limit_price=%s", limit)
	}
	r.since = time.Now().UTC()
	r.closeSentAt = r.since
	r.report.Command(protocol.SubjectStrategyExecutionClose, r.tag, string(mode), detail)
	if err := r.observer.Publish(protocol.SubjectStrategyExecutionClose, request); err != nil {
		return protocol.ExecutionCloseResult{}, err
	}
	message, err := r.observer.WaitForResult(ctx, protocol.SubjectExecutionCloseResult, r.tag, r.since, r.config.CaseTimeout)
	if err != nil {
		return protocol.ExecutionCloseResult{}, err
	}
	r.report.Evidence(message)
	var result protocol.ExecutionCloseResult
	if err := json.Unmarshal(message.Payload, &result); err != nil {
		return result, err
	}
	return result, nil
}

func (r *Runner) waitPosition(ctx context.Context, tag string, shouldExist bool) error {
	deadline := time.NewTimer(r.config.PositionTimeout)
	defer deadline.Stop()
	for {
		response, err := r.query(ctx, "position")
		if err == nil && r.hasLanePosition(response, tag) == shouldExist {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("position did not reach expected state exists=%t", shouldExist)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (r *Runner) hasLanePosition(response protocol.PositionQueryResponse, tag string) bool {
	for _, position := range response.Positions {
		if position.UniqueTag == tag && position.ConditionID == r.config.ConditionID && position.TokenID == r.config.AssetID && position.HasPosition {
			return true
		}
	}
	return false
}

func (r *Runner) lanePosition(response protocol.PositionQueryResponse) (protocol.PositionFeature, bool) {
	for _, position := range response.Positions {
		if position.UniqueTag == r.tag && position.ConditionID == r.config.ConditionID && position.TokenID == r.config.AssetID {
			return position, true
		}
	}
	return protocol.PositionFeature{}, false
}

func (r *Runner) query(ctx context.Context, purpose string) (protocol.PositionQueryResponse, error) {
	response, err := r.observer.Query(ctx, protocol.PositionQueryRequest{SchemaVersion: protocol.SchemaVersionV1, ConditionID: r.config.ConditionID})
	if err != nil {
		return response, fmt.Errorf("%s position query: %w", purpose, err)
	}
	return response, nil
}

func resultForTag(tag string) func([]byte) bool {
	return func(payload []byte) bool {
		var result struct {
			UniqueTag string `json:"unique_tag"`
		}
		return json.Unmarshal(payload, &result) == nil && result.UniqueTag == tag
	}
}

func (r *Runner) invalidSchemaCase(ctx context.Context) error {
	tag := fmt.Sprintf("%s-invalid-%d", r.config.strategy(), time.Now().UnixNano())
	request := protocol.ExecutionOpenRequest{
		SchemaVersion: "execution.invalid", UniqueTag: tag, Strategy: r.config.strategy(),
		ConditionID: r.config.ConditionID, TokenID: r.config.AssetID, Outcome: r.config.Outcome,
		Side: protocol.SideBuy, TargetUSD: r.config.TargetUSD, LimitPrice: r.config.BuyLimit,
		TimeInForce: protocol.TimeInForceGTC, ExpiresAt: time.Now().UTC().Add(r.config.CaseTimeout),
		Policy: protocol.ExecutionPolicy{Style: protocol.ExecutionStyleLimit},
	}
	r.report.Command(protocol.SubjectStrategyExecutionOpen, tag, "INVALID_SCHEMA", "non-funding protocol rejection probe")
	if err := r.observer.Publish(protocol.SubjectStrategyExecutionOpen, request); err != nil {
		return err
	}
	message, err := r.observer.WaitFor(ctx, protocol.SubjectExecutionOpenResult, resultForTag(tag), r.config.CaseTimeout)
	if err != nil {
		return err
	}
	r.report.Evidence(message)
	var result protocol.ExecutionOpenResult
	if err := json.Unmarshal(message.Payload, &result); err != nil {
		return err
	}
	if result.Status != protocol.ResultFailed || result.ReasonCode != protocol.ReasonInvalidIntent {
		return fmt.Errorf("expected INVALID_INTENT rejection, got status=%s reason_code=%s", result.Status, result.ReasonCode)
	}
	return nil
}

func hasAssetPosition(response protocol.PositionQueryResponse, assetID string) bool {
	for _, position := range response.Positions {
		if position.TokenID == assetID && position.HasPosition {
			return true
		}
	}
	return false
}
