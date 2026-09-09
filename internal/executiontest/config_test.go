package executiontest

import (
	"strings"
	"testing"
	"time"
)

func TestConfigValidateRejectsBuyLimitOutsideUnitRange(t *testing.T) {
	config := validConfig()
	config.BuyLimit = "4.2"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "buy limit") {
		t.Fatalf("Validate() error = %v, want buy limit error", err)
	}
}

func TestConfigValidateRejectsNonPositiveTarget(t *testing.T) {
	config := validConfig()
	config.TargetUSD = "0"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "target USD") {
		t.Fatalf("Validate() error = %v, want target USD error", err)
	}
}

func TestConfigValidateRequiresMarketIdentity(t *testing.T) {
	config := validConfig()
	config.AssetID = ""
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "asset ID") {
		t.Fatalf("Validate() error = %v, want market identity error", err)
	}
}

func TestConfigValidateAcceptsExplicitSafeConfiguration(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("Validate() unexpected error: %v", err)
	}
}

func validConfig() Config {
	return Config{
		NATSURL: "nats://127.0.0.1:4222", ConditionID: "0xcondition", AssetID: "123", Outcome: "Up",
		TargetUSD: "1", BuyLimit: "0.42", ReportDir: "reports",
		CaseTimeout: time.Minute, CleanupTimeout: time.Second,
	}
}
