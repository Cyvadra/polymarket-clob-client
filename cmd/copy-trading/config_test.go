package main

import (
	"testing"
	"time"
)

func TestConfigFromEnvDefaults(t *testing.T) {
	t.Setenv("COPY_TRADING_USD", "10")
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.USD != 10 || cfg.InitialDiff != 0.02 || cfg.Subject != "pmm.user.activity" || cfg.MaxTradeAge != 10*time.Second || cfg.DryRun {
		t.Fatalf("unexpected defaults %+v", cfg)
	}
}

func TestConfigFromEnvRejectsBadValues(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"missing usd":    {},
		"negative usd":   {"COPY_TRADING_USD": "-1"},
		"diff too large": {"COPY_TRADING_USD": "10", "COPY_TRADING_INITIAL_DIFF": "1"},
		"negative diff":  {"COPY_TRADING_USD": "10", "COPY_TRADING_INITIAL_DIFF": "-0.01"},
		"bad dry run":    {"COPY_TRADING_USD": "10", "COPY_TRADING_DRY_RUN": "maybe"},
		"bad max age":    {"COPY_TRADING_USD": "10", "COPY_TRADING_MAX_TRADE_AGE": "soon"},
	} {
		for k, v := range env {
			t.Setenv(k, v)
		}
		if _, err := configFromEnv(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		for k := range env {
			t.Setenv(k, "")
		}
	}
}
