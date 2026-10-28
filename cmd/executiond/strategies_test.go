package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
)

// withStrategyFile writes body to a strategy file and points the config at
// it in place of the allowlist TestMain sets.
func withStrategyFile(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "strategies.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"EXECUTION_STRATEGIES_FILE":    path,
		"EXECUTION_ALLOWED_STRATEGIES": "",
		"EXECUTION_DRAWDOWN_START":     "2026-10-19T06:30:00Z",
	})
}

func TestConfigFromEnvReadsStrategyFile(t *testing.T) {
	withStrategyFile(t, `{
		"default_max_drawdown": "20%",
		"strategies": {
			"LockInDS":        {"max_drawdown": "$80"},
			"SpotVolPrecap5m": {"max_drawdown": "$80", "size_multiplier": 2.5},
			"CalmFavSimp":     {"size_multiplier": 0.5},
			"FairDelta":       {"size_multiplier": 1}
		}
	}`)
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	for _, name := range []string{"LockInDS", "SpotVolPrecap5mBTC", "CalmFavSimpUp", "CalmFavSimpDown", "FairDelta"} {
		if !cfg.AllowedStrategies.Allows(name) {
			t.Fatalf("%s not allowed", name)
		}
	}
	if cfg.AllowedStrategies.Allows("FairPriced") {
		t.Fatal("FairPriced is not listed")
	}
	if len(cfg.StrategyMaxDrawdown) != 2 || cfg.StrategyMaxDrawdown["SpotVolPrecap5m"] != equity.DollarLimit(80) {
		t.Fatalf("limits=%+v", cfg.StrategyMaxDrawdown)
	}
	if cfg.StrategyDefaultMaxDrawdown != equity.FractionLimit(0.2) {
		t.Fatalf("default=%+v", cfg.StrategyDefaultMaxDrawdown)
	}
	// A multiplier of 1 is no multiplier.
	if len(cfg.SizeMultipliers) != 2 || cfg.SizeMultipliers["SpotVolPrecap5m"] != "2.5" || cfg.SizeMultipliers["CalmFavSimp"] != "0.5" {
		t.Fatalf("multipliers=%+v", cfg.SizeMultipliers)
	}
}

func TestConfigFromEnvRejectsBadStrategyFiles(t *testing.T) {
	for name, body := range map[string]string{
		"up/down pair":     `{"strategies": {"CalmFavSimpUp": {}, "CalmFavSimpDown": {}}}`,
		"overlap":          `{"strategies": {"LockIn": {}, "LockInDS": {}}}`,
		"none":             `{"strategies": {}}`,
		"unknown field":    `{"strategies": {"LockInDS": {"max_drawdwn": "$80"}}}`,
		"unknown top":      `{"allowed": ["LockInDS"], "strategies": {"LockInDS": {}}}`,
		"zero multiplier":  `{"strategies": {"LockInDS": {"size_multiplier": 0}}}`,
		"negative":         `{"strategies": {"LockInDS": {"size_multiplier": -2}}}`,
		"bad limit":        `{"strategies": {"LockInDS": {"max_drawdown": "80"}}}`,
		"wildcard limit":   `{"strategies": {"*": {"max_drawdown": "$80"}}}`,
		"wildcard and one": `{"strategies": {"*": {}, "LockInDS": {}}}`,
		"padded key":       `{"strategies": {" LockInDS": {}}}`,
		"not json":         `LockInDS`,
		"trailing":         `{"strategies": {"LockInDS": {}}} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			withStrategyFile(t, body)
			if _, err := configFromEnv(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestConfigFromEnvRefusesStrategyFileAlongsideEnv(t *testing.T) {
	for _, name := range strategyEnvVars {
		t.Run(name, func(t *testing.T) {
			withStrategyFile(t, `{"strategies": {"LockInDS": {}}}`)
			withEnv(t, map[string]string{name: "LockInDS"})
			if _, err := configFromEnv(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestConfigFromEnvRejectsUpDownPairInEnv(t *testing.T) {
	withEnv(t, map[string]string{"EXECUTION_ALLOWED_STRATEGIES": "LockInDS,CalmFavSimpUp,CalmFavSimpDown"})
	if _, err := configFromEnv(); err == nil {
		t.Fatal("expected an error")
	}
}
