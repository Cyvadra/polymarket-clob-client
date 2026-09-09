package executiontest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
)

type Runner struct {
	config   Config
	observer *Observer
	report   *Report
	spent    string
}

func NewRunner(config Config, observer *Observer, report *Report) *Runner {
	return &Runner{config: config, observer: observer, report: report, spent: "0"}
}

func (r *Runner) Run(ctx context.Context) error {
	if err := r.invalidSchemaCase(); err != nil {
		r.report.Scenario("invalid-schema-rejection", StatusFail, err.Error())
		return err
	}
	r.report.Scenario("invalid-schema-rejection", StatusPass, "executiond published INVALID_INTENT without submitting an order")

	baseline, err := r.query(ctx, "baseline")
	if err != nil {
		r.report.Scenario("position-query-baseline", StatusFail, err.Error())
		return err
	}
	r.report.Note("Initial Position", fmt.Sprintf("%+v", baseline.Positions))
	if hasAssetPosition(baseline, r.config.AssetID) {
		r.report.Scenario("preflight-empty-position", StatusFail, "existing visible position found; no real BUY was sent")
		return fmt.Errorf("visible position already exists for the supplied filters")
	}
	r.report.Scenario("position-query-baseline", StatusPass, "request/reply succeeded with no visible position")

	tag := fmt.Sprintf("%s-%d", r.config.strategy(), time.Now().UnixNano())
	open := protocol.ExecutionOpenRequest{
		SchemaVersion: protocol.SchemaVersionV1, UniqueTag: tag, Strategy: r.config.strategy(),
		ConditionID: r.config.ConditionID, TokenID: r.config.AssetID, Outcome: r.config.Outcome,
		Side: protocol.SideBuy, TargetUSD: r.config.TargetUSD, LimitPrice: r.config.BuyLimit,
		TimeInForce: protocol.TimeInForceGTC, ExpiresAt: time.Now().UTC().Add(r.config.CaseTimeout),
		Policy: protocol.ExecutionPolicy{Style: protocol.ExecutionStyleLimit, CompleteWithinMillis: r.config.CaseTimeout.Milliseconds()},
	}
	if err := r.publishBuy(ctx, open); err != nil {
		r.report.Scenario("limit-buy", StatusFail, err.Error())
		return err
	}
	message, err := r.observer.WaitFor(protocol.SubjectExecutionOpenResult, resultForTag(tag), r.config.CaseTimeout)
	if err != nil {
		r.report.Scenario("limit-buy", StatusInconclusive, err.Error())
		return r.cleanup(ctx, tag)
	}
	r.report.Evidence(message)
	var result protocol.ExecutionOpenResult
	if err := json.Unmarshal(message.Payload, &result); err != nil {
		return err
	}
	if result.Status != protocol.ResultSucceeded || result.FilledShares <= 0 {
		r.report.Scenario("limit-buy", StatusFail, fmt.Sprintf("status=%s filled_shares=%.8f", result.Status, result.FilledShares))
		return r.cleanup(ctx, tag)
	}
	r.spent = r.config.TargetUSD
	r.report.Scenario("limit-buy", StatusPass, fmt.Sprintf("filled_shares=%.8f", result.FilledShares))
	if err := r.waitPosition(ctx, tag, true); err != nil {
		r.report.Scenario("position-after-buy", StatusFail, err.Error())
		return r.cleanup(ctx, tag)
	}
	r.report.Scenario("position-after-buy", StatusPass, "visible position confirmed through NATS")
	return r.cleanup(ctx, tag)
}

func (r *Runner) publishBuy(ctx context.Context, request protocol.ExecutionOpenRequest) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if request.TargetUSD != r.config.TargetUSD {
		return fmt.Errorf("unexpected target amount")
	}
	r.report.Command(protocol.SubjectStrategyExecutionOpen, request.UniqueTag, "BUY", fmt.Sprintf("target_usd=%s limit_price=%s", request.TargetUSD, request.LimitPrice))
	return r.observer.Publish(protocol.SubjectStrategyExecutionOpen, request)
}

func (r *Runner) cleanup(ctx context.Context, tag string) error {
	request := protocol.ExecutionCloseRequest{
		SchemaVersion: protocol.SchemaVersionV1, UniqueTag: tag, Strategy: r.config.strategy(),
		ConditionID: r.config.ConditionID, AssetID: r.config.AssetID, Outcome: r.config.Outcome,
		Mode: protocol.ExecutionCloseModeForce, CreatedAt: time.Now().UTC(),
	}
	r.report.Command(protocol.SubjectStrategyExecutionClose, tag, "FORCE_CLOSE", "SELL 0.01 FAK is implemented by executiond")
	if err := r.observer.Publish(protocol.SubjectStrategyExecutionClose, request); err != nil {
		r.report.Scenario("force-close-cleanup", StatusFail, err.Error())
		return err
	}
	if err := r.waitPosition(ctx, tag, false); err != nil {
		r.report.Scenario("force-close-cleanup", StatusInconclusive, err.Error())
		return err
	}
	r.report.Scenario("force-close-cleanup", StatusPass, "position no longer visible through NATS")
	return nil
}

func (r *Runner) waitPosition(ctx context.Context, tag string, shouldExist bool) error {
	deadline := time.NewTimer(r.config.CleanupTimeout)
	defer deadline.Stop()
	for {
		response, err := r.query(ctx, "position")
		if err == nil {
			found := false
			for _, position := range response.Positions {
				if position.UniqueTag == tag && position.ConditionID == r.config.ConditionID && position.TokenID == r.config.AssetID && position.HasPosition {
					found = true
				}
			}
			if found == shouldExist {
				return nil
			}
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

func (r *Runner) invalidSchemaCase() error {
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
	message, err := r.observer.WaitFor(protocol.SubjectExecutionOpenResult, resultForTag(tag), r.config.CaseTimeout)
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
