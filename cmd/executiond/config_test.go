package main

import (
	"os"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
)

// Every config needs the strategies the wallet trades; tests of other
// settings run with one in place.
func TestMain(m *testing.M) {
	os.Setenv("EXECUTION_ALLOWED_STRATEGIES", "strategy")
	os.Exit(m.Run())
}

func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestConfigFromEnvDefaults(t *testing.T) {
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if cfg.MaxTradeAge != 24*time.Hour {
		t.Fatalf("expected default max trade age of 24h, got %s", cfg.MaxTradeAge)
	}
}

func TestConfigFromEnvAcceptsDurationAndMillisecondForms(t *testing.T) {
	withEnv(t, map[string]string{"EXECUTION_RECONCILE_MAX_TRADE_AGE": "48h"})
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if cfg.MaxTradeAge != 48*time.Hour {
		t.Fatalf("expected 48h, got %s", cfg.MaxTradeAge)
	}

	withEnv(t, map[string]string{"EXECUTION_RECONCILE_MAX_TRADE_AGE": "1000"})
	cfg, err = configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if cfg.MaxTradeAge != 1000*time.Millisecond {
		t.Fatalf("expected 1000ms, got %s", cfg.MaxTradeAge)
	}
}

func TestConfigFromEnvRejectsZeroMaxTradeAge(t *testing.T) {
	withEnv(t, map[string]string{"EXECUTION_RECONCILE_MAX_TRADE_AGE": "0s"})
	if _, err := configFromEnv(); err == nil {
		t.Fatal("expected error for zero max trade age")
	}
}

func TestConfigFromEnvRejectsInvalidMaxTradeAge(t *testing.T) {
	withEnv(t, map[string]string{"EXECUTION_RECONCILE_MAX_TRADE_AGE": "not-a-duration"})
	if _, err := configFromEnv(); err == nil {
		t.Fatal("expected error for unparsable max trade age")
	}
}

func TestConfigFromEnvRejectsNegativeMaxTradeAge(t *testing.T) {
	withEnv(t, map[string]string{"EXECUTION_RECONCILE_MAX_TRADE_AGE": "-1h"})
	if _, err := configFromEnv(); err == nil {
		t.Fatal("expected error for negative max trade age")
	}
}

func TestConfigFromEnvDrawdownLimit(t *testing.T) {
	withEnv(t, map[string]string{"EXECUTION_MAX_DRAWDOWN": "20%", "EXECUTION_DRAWDOWN_START": "2026-09-28"})
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if cfg.MaxDrawdown != equity.FractionLimit(0.2) || !cfg.DrawdownStart.Equal(time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)) || cfg.EquityFlowThresholdUSD != 1 {
		t.Fatalf("cfg=%+v", cfg)
	}

	withEnv(t, map[string]string{"EXECUTION_DRAWDOWN_START": "2026-09-28T08:00:00+08:00"})
	if cfg, err = configFromEnv(); err != nil || !cfg.DrawdownStart.Equal(time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("RFC 3339 start: %v %v", cfg.DrawdownStart, err)
	}

	// Older configs hold a bare fraction; it still reads as one.
	withEnv(t, map[string]string{"EXECUTION_MAX_DRAWDOWN": "0.2"})
	if cfg, err = configFromEnv(); err != nil || cfg.MaxDrawdown != equity.FractionLimit(0.2) {
		t.Fatalf("bare fraction: %+v %v", cfg.MaxDrawdown, err)
	}

	withEnv(t, map[string]string{"EXECUTION_MAX_DRAWDOWN": " $ 123.5 "})
	if cfg, err = configFromEnv(); err != nil || cfg.MaxDrawdown != equity.DollarLimit(123.5) {
		t.Fatalf("dollar limit: %+v %v", cfg.MaxDrawdown, err)
	}

	for name, kv := range map[string]map[string]string{
		"no start":       {"EXECUTION_DRAWDOWN_START": ""},
		"bad start":      {"EXECUTION_DRAWDOWN_START": "yesterday"},
		"bare 1":         {"EXECUTION_MAX_DRAWDOWN": "1"},
		"bare number":    {"EXECUTION_MAX_DRAWDOWN": "20"},
		"100 percent":    {"EXECUTION_MAX_DRAWDOWN": "100%"},
		"zero percent":   {"EXECUTION_MAX_DRAWDOWN": "0%"},
		"zero dollars":   {"EXECUTION_MAX_DRAWDOWN": "$0"},
		"bad dollars":    {"EXECUTION_MAX_DRAWDOWN": "$x"},
		"nan dollars":    {"EXECUTION_MAX_DRAWDOWN": "$NaN"},
		"nan percent":    {"EXECUTION_MAX_DRAWDOWN": "NaN%"},
		"inf dollars":    {"EXECUTION_MAX_DRAWDOWN": "$Inf"},
		"negative flows": {"EXECUTION_EQUITY_FLOW_THRESHOLD_USD": "-1"},
	} {
		t.Run(name, func(t *testing.T) {
			withEnv(t, map[string]string{"EXECUTION_MAX_DRAWDOWN": "20%", "EXECUTION_DRAWDOWN_START": "2026-09-28"})
			withEnv(t, kv)
			if _, err := configFromEnv(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestConfigFromEnvRequiresAllowedStrategies(t *testing.T) {
	for _, value := range []string{"", " ", ","} {
		withEnv(t, map[string]string{"EXECUTION_ALLOWED_STRATEGIES": value})
		if _, err := configFromEnv(); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestConfigFromEnvReadsAllowedStrategies(t *testing.T) {
	withEnv(t, map[string]string{"EXECUTION_ALLOWED_STRATEGIES": "late-gap, momentum"})
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if !cfg.AllowedStrategies.Allows("late-gap") || !cfg.AllowedStrategies.Allows("momentum") || cfg.AllowedStrategies.Allows("other") {
		t.Fatalf("allowed=%v", cfg.AllowedStrategies.Names())
	}
}

func TestConfigFromEnvStrategyDrawdownLimits(t *testing.T) {
	withEnv(t, map[string]string{
		"EXECUTION_ALLOWED_STRATEGIES":    "late-gap,momentum,ns:v2",
		"EXECUTION_STRATEGY_MAX_DRAWDOWN": " late-gap:10% , ns:v2:$150 ",
		"EXECUTION_DRAWDOWN_START":        "2026-09-28",
	})
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if !cfg.MaxDrawdown.IsZero() || len(cfg.StrategyMaxDrawdown) != 2 || cfg.StrategyMaxDrawdown["late-gap"] != equity.FractionLimit(0.1) || cfg.StrategyMaxDrawdown["ns:v2"] != equity.DollarLimit(150) {
		t.Fatalf("limits %+v, global %+v", cfg.StrategyMaxDrawdown, cfg.MaxDrawdown)
	}

	for name, kv := range map[string]map[string]string{
		"no start":    {"EXECUTION_DRAWDOWN_START": ""},
		"not allowed": {"EXECUTION_STRATEGY_MAX_DRAWDOWN": "Late-Gap:10%"},
		"duplicate":   {"EXECUTION_STRATEGY_MAX_DRAWDOWN": "late-gap:10%,late-gap:$20"},
		"no limit":    {"EXECUTION_STRATEGY_MAX_DRAWDOWN": "late-gap"},
		"no name":     {"EXECUTION_STRATEGY_MAX_DRAWDOWN": ":10%"},
		"bare":        {"EXECUTION_STRATEGY_MAX_DRAWDOWN": "late-gap:10"},
		"100 percent": {"EXECUTION_STRATEGY_MAX_DRAWDOWN": "late-gap:100%"},
		"only commas": {"EXECUTION_STRATEGY_MAX_DRAWDOWN": ","},
	} {
		t.Run(name, func(t *testing.T) {
			withEnv(t, kv)
			if _, err := configFromEnv(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestConfigFromEnvAllowsEveryStrategyWithAWildcard(t *testing.T) {
	withEnv(t, map[string]string{
		"EXECUTION_ALLOWED_STRATEGIES":    " * ",
		"EXECUTION_STRATEGY_MAX_DRAWDOWN": "late-gap:10%",
		"EXECUTION_DRAWDOWN_START":        "2026-09-28",
	})
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if !cfg.AllowedStrategies.Wildcard() || !cfg.AllowedStrategies.Allows("anything") || cfg.StrategyMaxDrawdown["late-gap"] != equity.FractionLimit(0.1) {
		t.Fatalf("allowed=%v limits=%+v", cfg.AllowedStrategies.Names(), cfg.StrategyMaxDrawdown)
	}
	withEnv(t, map[string]string{"EXECUTION_ALLOWED_STRATEGIES": "*,late-gap"})
	if _, err := configFromEnv(); err == nil {
		t.Fatal("expected * alongside a name to be rejected")
	}
}

func TestConfigFromEnvStrategyDefaultDrawdown(t *testing.T) {
	for value, want := range map[string]equity.DrawdownLimit{"15%": equity.FractionLimit(0.15), "$250": equity.DollarLimit(250)} {
		withEnv(t, map[string]string{
			"EXECUTION_STRATEGY_DEFAULT_MAX_DRAWDOWN": value,
			"EXECUTION_DRAWDOWN_START":                "2026-09-28",
		})
		cfg, err := configFromEnv()
		if err != nil {
			t.Fatalf("%s: %v", value, err)
		}
		if cfg.StrategyDefaultMaxDrawdown != want || !cfg.drawdownLimited() {
			t.Fatalf("%s read as %+v", value, cfg.StrategyDefaultMaxDrawdown)
		}
	}
	for name, kv := range map[string]map[string]string{
		"no start":          {"EXECUTION_STRATEGY_DEFAULT_MAX_DRAWDOWN": "10%", "EXECUTION_DRAWDOWN_START": ""},
		"invalid":           {"EXECUTION_STRATEGY_DEFAULT_MAX_DRAWDOWN": "10", "EXECUTION_DRAWDOWN_START": "2026-09-28"},
		"wildcard per-name": {"EXECUTION_STRATEGY_MAX_DRAWDOWN": "*:10%", "EXECUTION_DRAWDOWN_START": "2026-09-28"},
	} {
		t.Run(name, func(t *testing.T) {
			withEnv(t, kv)
			if _, err := configFromEnv(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
