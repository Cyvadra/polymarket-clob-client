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
	NATSURL        string
	ConditionID    string
	AssetID        string
	Outcome        string
	TargetUSD      string
	BuyLimit       string
	SellLimit      string
	Strategy       string
	ReportDir      string
	CaseTimeout    time.Duration
	CleanupTimeout time.Duration
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
	if c.CaseTimeout <= 0 || c.CleanupTimeout <= 0 {
		return fmt.Errorf("case and cleanup timeouts must be positive")
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
