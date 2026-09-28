package main

import (
	"os"
	"testing"
	"time"
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
	withEnv(t, map[string]string{"EXECUTION_MAX_DRAWDOWN": "0.2", "EXECUTION_DRAWDOWN_START": "2026-09-28"})
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if cfg.MaxDrawdown != 0.2 || !cfg.DrawdownStart.Equal(time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)) || cfg.EquityFlowThresholdUSD != 1 {
		t.Fatalf("cfg=%+v", cfg)
	}

	withEnv(t, map[string]string{"EXECUTION_DRAWDOWN_START": "2026-09-28T08:00:00+08:00"})
	if cfg, err = configFromEnv(); err != nil || !cfg.DrawdownStart.Equal(time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("RFC 3339 start: %v %v", cfg.DrawdownStart, err)
	}

	for name, kv := range map[string]map[string]string{
		"no start":       {"EXECUTION_DRAWDOWN_START": ""},
		"bad start":      {"EXECUTION_DRAWDOWN_START": "yesterday"},
		"limit of 1":     {"EXECUTION_MAX_DRAWDOWN": "1"},
		"percent":        {"EXECUTION_MAX_DRAWDOWN": "20"},
		"negative flows": {"EXECUTION_EQUITY_FLOW_THRESHOLD_USD": "-1"},
	} {
		t.Run(name, func(t *testing.T) {
			withEnv(t, map[string]string{"EXECUTION_MAX_DRAWDOWN": "0.2", "EXECUTION_DRAWDOWN_START": "2026-09-28"})
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
