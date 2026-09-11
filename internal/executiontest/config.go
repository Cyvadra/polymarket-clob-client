// Package executiontest implements a NATS-only black-box integration test runner.
package executiontest

import (
	"fmt"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
)

const defaultStrategy = "executiontest"

// Config contains every value that can cause the runner to send a real order.
// The caller must provide market identity and limits explicitly; the runner
// never discovers or guesses an asset from a condition ID.
type Config struct {
	NATSURL         string
	ConditionID     string
	AssetID         string
	Outcome         string
	TargetUSD       string
	BuyLimit        string
	SellLimit       string
	Strategy        string
	ReportDir       string
	CaseTimeout     time.Duration
	ResultGrace     time.Duration
	CleanupTimeout  time.Duration
	PositionTimeout time.Duration
	QueryTimeout    time.Duration
	Hold            time.Duration
	CloseMode       string
	Negative        bool
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.NATSURL) == "" {
		return fmt.Errorf("NATS URL is required")
	}
	if strings.TrimSpace(c.ConditionID) == "" || strings.TrimSpace(c.AssetID) == "" || strings.TrimSpace(c.Outcome) == "" {
		return fmt.Errorf("condition ID, asset ID, and outcome are required")
	}
	if _, err := decimal.PositiveFloat(c.TargetUSD); err != nil {
		return fmt.Errorf("target USD must be a positive decimal: %w", err)
	}
	if _, err := decimal.Price(c.BuyLimit); err != nil {
		return fmt.Errorf("buy limit must be a price in (0,1): %w", err)
	}
	if strings.TrimSpace(c.SellLimit) != "" {
		if _, err := decimal.Price(c.SellLimit); err != nil {
			return fmt.Errorf("sell limit must be a price in (0,1): %w", err)
		}
	}
	if c.CaseTimeout <= 0 || c.CleanupTimeout <= 0 || c.PositionTimeout <= 0 || c.QueryTimeout <= 0 {
		return fmt.Errorf("case, cleanup, position, and query timeouts must be positive")
	}
	if c.ResultGrace < 0 {
		return fmt.Errorf("result grace must not be negative")
	}
	if c.Hold < 0 {
		return fmt.Errorf("hold must not be negative")
	}
	switch strings.TrimSpace(c.CloseMode) {
	case "", "auto", "limit", "force":
	default:
		return fmt.Errorf("close mode must be one of auto, limit, force")
	}
	if strings.TrimSpace(c.ReportDir) == "" {
		return fmt.Errorf("report directory is required")
	}
	return nil
}

func (c Config) strategy() string {
	if strategy := strings.TrimSpace(c.Strategy); strategy != "" {
		return strategy
	}
	return defaultStrategy
}

// resultWait is how long to wait for an OPEN or CLOSE terminal result. It runs
// past CaseTimeout because executiond only cancels a resting order once its
// own complete_within_ms deadline (which the runner sets to CaseTimeout)
// passes, and that cancel plus the FAILED result it produces take a few more
// seconds to arrive.
func (c Config) resultWait() time.Duration {
	return c.CaseTimeout + c.ResultGrace
}
