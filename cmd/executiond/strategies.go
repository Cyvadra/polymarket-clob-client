package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/nats"
	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
)

// strategyFile is the JSON EXECUTION_STRATEGIES_FILE names: the strategies
// this wallet trades, keyed by name prefix, each with its own settings.
type strategyFile struct {
	MaxOpenBuyNotionalUSD  json.Number                 `json:"max_open_buy_notional_usd"`
	MaxDrawdown            string                      `json:"max_drawdown"`
	EquityFlowThresholdUSD json.Number                 `json:"equity_flow_threshold_usd"`
	DefaultMaxDrawdown     string                      `json:"default_max_drawdown"`
	DrawdownStart          string                      `json:"drawdown_start"`
	SizeAfterLossOnly      *bool                       `json:"size_after_loss_only"`
	Strategies             map[string]strategySettings `json:"strategies"`
}

type strategySettings struct {
	MaxDrawdown    string      `json:"max_drawdown"`
	SizeMultiplier json.Number `json:"size_multiplier"`
}

// strategyEnvVars are the per-strategy settings the file replaces; setting
// both would leave two sources to disagree.
var strategyEnvVars = []string{"EXECUTION_MAX_OPEN_BUY_NOTIONAL_USD", "EXECUTION_MAX_DRAWDOWN", "EXECUTION_EQUITY_FLOW_THRESHOLD_USD", "EXECUTION_DRAWDOWN_START", "EXECUTION_SIZE_AFTER_LOSS_ONLY", "EXECUTION_ALLOWED_STRATEGIES", "EXECUTION_STRATEGY_MAX_DRAWDOWN", "EXECUTION_STRATEGY_DEFAULT_MAX_DRAWDOWN"}

// loadStrategyFile reads the strategy file into cfg. Its keys are the
// allowlist, so they follow the allowlist's rules: prefixes that do not
// overlap, no Up/Down pair, and "*" only alone. It returns the limits
// written as bare fractions, for the caller to warn of.
func loadStrategyFile(path string, cfg *config) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var file strategyFile
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("parse %s: trailing data after the JSON object", path)
	}
	var bares []string
	if value := file.MaxOpenBuyNotionalUSD.String(); value != "" {
		if !decimal.Positive(value) {
			return nil, fmt.Errorf("max_open_buy_notional_usd %s must be a positive number", value)
		}
		cfg.MaxOpenBuyNotionalUSD = value
	}
	if value := strings.TrimSpace(file.MaxDrawdown); value != "" {
		limit, bare, err := parseDrawdownLimit(value)
		if err != nil {
			return nil, fmt.Errorf("max_drawdown: %w", err)
		}
		if bare {
			bares = append(bares, value)
		}
		cfg.MaxDrawdown = limit
	}
	if value := file.EquityFlowThresholdUSD.String(); value != "" {
		threshold, err := strconv.ParseFloat(value, 64)
		if err != nil || threshold < 0 {
			return nil, fmt.Errorf("equity_flow_threshold_usd %s must be a non-negative number", value)
		}
		cfg.EquityFlowThresholdUSD = threshold
	}
	if value := strings.TrimSpace(file.DrawdownStart); value != "" {
		start, err := parseStart(value)
		if err != nil {
			return nil, fmt.Errorf("drawdown_start: %w", err)
		}
		cfg.DrawdownStart = start
	}
	if file.SizeAfterLossOnly != nil {
		cfg.SizeAfterLossOnly = *file.SizeAfterLossOnly
	}
	names := make([]string, 0, len(file.Strategies))
	for name := range file.Strategies {
		if name != strings.TrimSpace(name) || name == "" {
			return nil, fmt.Errorf("strategy key %q must be a name prefix without surrounding spaces", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	allowed, err := nats.NewAllowlist(names)
	if err != nil {
		return nil, fmt.Errorf("strategies: %w", err)
	}
	cfg.AllowedStrategies = allowed
	for _, name := range names {
		settings := file.Strategies[name]
		if value := strings.TrimSpace(settings.MaxDrawdown); value != "" {
			if name == nats.Wildcard {
				return nil, fmt.Errorf("strategy %q: set a limit for every strategy with default_max_drawdown", name)
			}
			limit, bare, err := parseDrawdownLimit(value)
			if err != nil {
				return nil, fmt.Errorf("strategy %q max_drawdown: %w", name, err)
			}
			if bare {
				bares = append(bares, value)
			}
			if cfg.StrategyMaxDrawdown == nil {
				cfg.StrategyMaxDrawdown = map[string]equity.DrawdownLimit{}
			}
			cfg.StrategyMaxDrawdown[name] = limit
		}
		if value := settings.SizeMultiplier.String(); value != "" {
			if !decimal.Positive(value) {
				return nil, fmt.Errorf("strategy %q size_multiplier %s must be a positive number", name, value)
			}
			if decimal.Compare(value, "1") != 0 {
				if cfg.SizeMultipliers == nil {
					cfg.SizeMultipliers = map[string]string{}
				}
				cfg.SizeMultipliers[name] = value
			}
		}
	}
	if value := strings.TrimSpace(file.DefaultMaxDrawdown); value != "" {
		limit, bare, err := parseDrawdownLimit(value)
		if err != nil {
			return nil, fmt.Errorf("default_max_drawdown: %w", err)
		}
		if bare {
			bares = append(bares, value)
		}
		cfg.StrategyDefaultMaxDrawdown = limit
	}
	return bares, nil
}
