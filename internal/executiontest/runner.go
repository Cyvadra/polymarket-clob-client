package executiontest

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
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
	// closeSentAt is when the first close of the run was sent. Later closes
	// (the auto fallback, the cleanup backstop) keep it, so fill
	// reconciliation covers every close leg.
	closeSentAt time.Time

	// positionSeen records whether the lane's position was ever visible. An
	// absent position after cleanup only proves the close worked if it was.
	positionSeen bool
	// positionClosed is set once the lane is confirmed flat through NATS, so
	// the final safety close is skipped instead of selling shares that are
	// already gone.
	positionClosed bool
	// forceFellBack is set when a FORCE_CLOSE ran after a LIMIT_CLOSE that did
	// not fully exit the lane. FORCE_CLOSE publishes no result, so once it has
	// run the total exit quantity and price are unknown and P&L cannot be
	// computed from NATS alone.
	forceFellBack bool
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
	if others := otherVisiblePositions(baseline, r.config.AssetID); len(others) > 0 {
		r.report.Phase("baseline-flat", StatusInfo, "", "", fmt.Sprintf("%d other visible position(s) in this condition not touched by the run: %s (redeem or close them separately)", len(others), strings.Join(others, "; ")))
	}
	r.report.Phase("baseline-flat", StatusPass, "", "", "request/reply succeeded with no visible position for the target asset")
	return nil
}

// otherVisiblePositions lists visible positions in the queried condition that
// are not the run's target asset. They do not fail the baseline (the run only
// touches its own lane and asset) but they are worth surfacing: a leftover
// from an earlier interrupted run, or the other outcome token, that an
// operator still needs to redeem or close by hand.
func otherVisiblePositions(response protocol.PositionQueryResponse, assetID string) []string {
	var others []string
	for _, position := range response.Positions {
		if position.TokenID == assetID || !position.HasPosition {
			continue
		}
		others = append(others, fmt.Sprintf("tag=%s outcome=%s token=%s size=%.8f", position.UniqueTag, position.Outcome, position.TokenID, position.PositionSize))
	}
	return others
}

func (r *Runner) open(ctx context.Context) error {
	request := protocol.ExecutionOpenRequest{
		SchemaVersion: protocol.SchemaVersionV1, UniqueTag: r.tag, Strategy: r.config.strategy(),
		ConditionID: r.config.ConditionID, TokenID: r.config.AssetID, Outcome: r.config.Outcome,
		Side: protocol.SideBuy, TargetUSD: r.config.TargetUSD, LimitPrice: r.config.BuyLimit,
		TimeInForce: protocol.TimeInForceGTC, ExpiresAt: time.Now().UTC().Add(r.config.CaseTimeout),
		Policy: protocol.ExecutionPolicy{Style: protocol.ExecutionStyleLimit, CompleteWithinMillis: r.config.CaseTimeout.Milliseconds()},
	}
	r.since = time.Now().UTC()
	r.openSentAt = r.since
	r.report.Command(protocol.SubjectStrategyExecutionOpen, r.tag, "BUY", fmt.Sprintf("target_usd=%s limit_price=%s", request.TargetUSD, request.LimitPrice))
	if err := r.observer.Publish(protocol.SubjectStrategyExecutionOpen, request); err != nil {
		r.report.Phase("open", StatusFail, "", "", err.Error())
		return err
	}
	message, err := r.observer.WaitForResult(ctx, protocol.SubjectExecutionOpenResult, r.tag, r.since, r.config.resultWait())
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
	// Every close leg together must exit the whole opened position, whichever
	// leg (limit, force fallback) actually sold it.
	r.reconcileFills("close-fills-reconcile", r.closeSentAt, r.openShares)
	return nil
}

// reconcileFills totals the shares matched by the orders seen on
// execution.order.event since the command was sent and compares the total
// against the expected shares. Missing events are INCONCLUSIVE (executiond may
// not emit per-fill events on this path); a real mismatch is FAIL, but never
// fatal, so the run still closes the position.
func (r *Runner) reconcileFills(name string, since time.Time, expected float64) {
	events := r.observer.OrderEventsBetween(since, time.Now().UTC(), r.tag)
	if len(events) == 0 {
		r.report.Phase(name, StatusInconclusive, "", "", "no execution.order.event observed for this lane")
		return
	}
	matched, orders := matchedShares(events)
	tolerance := math.Max(1e-6, expected*0.01)
	detail := fmt.Sprintf("%d order event(s) across %d order(s), Σ matched_shares=%.8f vs expected shares=%.8f", len(events), orders, matched, expected)
	if math.Abs(matched-expected) > tolerance {
		r.report.Phase(name, StatusFail, fmt.Sprintf("%.8f", matched), "", detail+" (outside tolerance)")
		return
	}
	r.report.Phase(name, StatusPass, fmt.Sprintf("%.8f", matched), "", detail)
}

// orderKey identifies the order an event belongs to. The intent ID is the key
// because events published before the exchange acknowledged the order carry
// no exchange order ID yet, and every executiond intent places one child
// order (a close retry uses a fresh intent).
func orderKey(event protocol.ExecutionOrderEvent) string {
	if event.IntentID != "" {
		return event.IntentID
	}
	return "order:" + event.ExchangeOrderID
}

// matchedShares totals the shares matched per order. Each order event carries
// the order's cumulative matched_shares, so an order counts its largest value
// once; summing every event would count a partial fill again at each later
// transition of the same order.
func matchedShares(events []protocol.ExecutionOrderEvent) (total float64, orders int) {
	perOrder := make(map[string]float64)
	for _, event := range events {
		key := orderKey(event)
		perOrder[key] = math.Max(perOrder[key], event.MatchedShares)
	}
	for _, shares := range perOrder {
		total += shares
	}
	return total, len(perOrder)
}

// residualOrders returns the last event of every order whose latest observed
// state is not terminal, in first-seen order.
func residualOrders(events []protocol.ExecutionOrderEvent) []protocol.ExecutionOrderEvent {
	var keys []string
	last := make(map[string]protocol.ExecutionOrderEvent)
	for _, event := range events {
		key := orderKey(event)
		if _, seen := last[key]; !seen {
			keys = append(keys, key)
		}
		last[key] = event
	}
	var residual []protocol.ExecutionOrderEvent
	for _, key := range keys {
		state := statemachine.State(strings.ToUpper(strings.TrimSpace(last[key].State)))
		if !statemachine.IsTerminal(state) {
			residual = append(residual, last[key])
		}
	}
	return residual
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
	if !ok || !feature.HasPosition {
		r.report.Phase("position-open", StatusInconclusive, "", "", "position cleared between the wait and the accounting read")
		return nil
	}
	drift := math.Abs(feature.PositionSize - r.openShares)
	entry := "nil"
	if feature.EntryPrice != nil {
		entry = fmt.Sprintf("%.6f", *feature.EntryPrice)
		if r.openAvg <= 0 {
			// The open result omits average_price when executiond had no
			// recorded fill price when it was published; the lane's entry
			// price is the same buy's cost basis.
			r.openAvg = *feature.EntryPrice
			r.report.Note("Open Price", fmt.Sprintf("open result carried no average_price; using position entry_price %s for P&L", entry))
		}
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
		// FORCE_CLOSE never emits execution.close.result; positionFlat confirms it.
		_ = r.closeOnce(ctx, protocol.ExecutionCloseModeForce, "")
		return nil
	}
	r.checkLimitCrossing(limit)
	if err := r.closeOnce(ctx, protocol.ExecutionCloseModeLimit, limit); err != nil {
		if r.config.CloseMode != "auto" {
			return nil
		}
		r.report.Phase("close", StatusInconclusive, "", "", fmt.Sprintf("LIMIT_CLOSE did not fully exit the lane (%v); falling back to FORCE_CLOSE", err))
		r.forceFellBack = true
		_ = r.closeOnce(ctx, protocol.ExecutionCloseModeForce, "")
	}
	return nil
}

// checkLimitCrossing records whether the sell limit crosses the best bid seen
// on pmm.market.quotes. A crossing limit sells into the bid at once, so the
// run exercises an immediate taker sell rather than a resting limit sell.
func (r *Runner) checkLimitCrossing(limit string) {
	price, err := decimal.Price(limit)
	if err != nil {
		return
	}
	quote, ok := r.observer.LatestQuote(r.config.AssetID)
	if !ok || quote.Bid <= 0 {
		r.report.Phase("close-limit-check", StatusInfo, "", limit, "no quote observed for the asset; cannot tell whether the limit rests or crosses")
		return
	}
	age := time.Since(quote.Timestamp).Round(time.Second)
	if price <= quote.Bid {
		r.report.Phase("close-limit-check", StatusInfo, "", limit, fmt.Sprintf("limit %s <= best bid %.4f (quote age %s): the sell crosses and fills immediately at the bid, it does not rest", limit, quote.Bid, age))
		return
	}
	r.report.Phase("close-limit-check", StatusInfo, "", limit, fmt.Sprintf("limit %s > best bid %.4f (quote age %s): the sell rests until the bid reaches it", limit, quote.Bid, age))
}

// closeOnce sends one close and records the phase row. It returns an error
// only to signal the auto fallback for LIMIT_CLOSE; the run itself is never
// stopped by a close that did not confirm, because finalCleanup and
// positionFlat are the backstop. FORCE_CLOSE never emits
// execution.close.result (see pkg/executor/close.go), so sendClose does not
// wait for one and this always reports the request as sent, not a fill.
func (r *Runner) closeOnce(ctx context.Context, mode protocol.ExecutionCloseMode, limit string) error {
	result, err := r.sendClose(ctx, mode, limit)
	if err != nil {
		r.report.Phase("close", StatusInconclusive, "", "", fmt.Sprintf("%s: %v", mode, err))
		return err
	}
	if mode == protocol.ExecutionCloseModeForce {
		r.report.Phase("close", StatusInconclusive, "", "", fmt.Sprintf("%s sent; no close result is ever published, confirming via position query", mode))
		return nil
	}
	if result.Status != protocol.ResultSucceeded || result.FilledShares <= 0 {
		r.report.Phase("close", StatusFail, fmt.Sprintf("%.8f", result.FilledShares), "", fmt.Sprintf("%s status=%s reason_code=%s reason=%s", mode, result.Status, result.ReasonCode, result.Reason))
		return fmt.Errorf("%s did not fill", mode)
	}
	r.closeShares, r.closeAvg = result.FilledShares, result.AveragePrice
	// A LIMIT_CLOSE can report SUCCEEDED on a partial fill (see
	// docs/protocol/nats-v1.md); the remainder still rests until its deadline.
	// Treat that as needing the FORCE_CLOSE fallback so the lane is actually
	// flattened rather than left open under a PASS.
	if mode == protocol.ExecutionCloseModeLimit && r.openShares > 0 {
		tolerance := math.Max(1e-6, r.openShares*0.01)
		if result.FilledShares+tolerance < r.openShares {
			r.report.Phase("close", StatusInconclusive, fmt.Sprintf("%.8f", r.closeShares), fmt.Sprintf("%.6f", r.closeAvg), fmt.Sprintf("%s partially filled %.8f of %.8f", mode, result.FilledShares, r.openShares))
			return fmt.Errorf("%s partially filled %.8f of %.8f", mode, result.FilledShares, r.openShares)
		}
	}
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
	if r.forceFellBack {
		r.report.Phase("pnl-summary", StatusSkip, "", "", "a FORCE_CLOSE fallback ran after a partial LIMIT_CLOSE; FORCE_CLOSE publishes no result so the total exit quantity and price are unknown")
		return nil
	}
	if r.openAvg <= 0 || r.closeAvg <= 0 {
		r.report.Phase("pnl-summary", StatusSkip, "", "", fmt.Sprintf("a leg has no known average price (open %.6f, close %.6f)", r.openAvg, r.closeAvg))
		return nil
	}
	notionalIn := r.openShares * r.openAvg
	notionalOut := r.closeShares * r.closeAvg
	gross := notionalOut - notionalIn
	perShare := r.closeAvg - r.openAvg
	r.report.Note("P&L Summary", fmt.Sprintf(
		"| Leg | Shares | Avg price | Notional |\n|---|---|---|---|\n| open (buy) | %.8f | %.6f | %.6f |\n| close (sell) | %.8f | %.6f | %.6f |\n\nGross P&L: `%.6f` USD  (%.6f USD/share).\n\n"+
			"This is a gross, pre-fee figure from the fill-weighted prices on the execution results. Polymarket charges no fee on markets whose `base_fee` is 0 (the common case, and consistent with the wallet showing shares × price exactly); where a market does charge, the fee is carried as `fee_rate_bps` on the account fill rows, which this NATS-only test does not read. Subtract fees from the trade records if the target market has a non-zero base fee.",
		r.openShares, r.openAvg, notionalIn, r.closeShares, r.closeAvg, notionalOut, gross, perShare))
	r.report.Phase("pnl-summary", StatusInfo, fmt.Sprintf("%.8f", r.closeShares), fmt.Sprintf("%.6f", perShare), fmt.Sprintf("gross (pre-fee) %.6f USD (in %.6f, out %.6f)", gross, notionalIn, notionalOut))
	return nil
}

func (r *Runner) noResidualOrders(ctx context.Context) error {
	events := r.observer.OrderEventsBetween(r.openSentAt, time.Now().UTC(), r.tag)
	if len(events) == 0 {
		r.report.Phase("no-residual-orders", StatusSkip, "", "", "no execution.order.event observed to inspect")
		return nil
	}
	_, orders := matchedShares(events)
	if residual := residualOrders(events); len(residual) > 0 {
		var states []string
		for _, event := range residual {
			states = append(states, fmt.Sprintf("%s=%s", orderKey(event), event.State))
		}
		r.report.Phase("no-residual-orders", StatusFail, "", "", fmt.Sprintf("%d of %d order(s) last seen non-terminal: %s", len(residual), orders, strings.Join(states, ", ")))
		return nil
	}
	r.report.Phase("no-residual-orders", StatusPass, "", "", fmt.Sprintf("all %d order(s) last seen in a terminal state", orders))
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
	if r.closeSentAt.IsZero() {
		r.closeSentAt = r.since
	}
	r.report.Command(protocol.SubjectStrategyExecutionClose, r.tag, string(mode), detail)
	if err := r.observer.Publish(protocol.SubjectStrategyExecutionClose, request); err != nil {
		return protocol.ExecutionCloseResult{}, err
	}
	if mode == protocol.ExecutionCloseModeForce {
		// executiond never publishes execution.close.result for FORCE_CLOSE.
		return protocol.ExecutionCloseResult{}, nil
	}
	message, err := r.observer.WaitForResult(ctx, protocol.SubjectExecutionCloseResult, r.tag, r.since, r.config.resultWait())
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
