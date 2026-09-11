package main

import (
	"testing"
	"time"
)

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
