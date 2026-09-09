package executiontest

import (
	"os"
	"strings"
	"testing"
)

func TestReportPersistsScenarioAndEvidence(t *testing.T) {
	report, err := NewReport(t.TempDir(), validConfig())
	if err != nil {
		t.Fatalf("NewReport() error: %v", err)
	}
	report.Scenario("limit-buy", StatusPass, "filled | confirmed")
	report.Command("strategy.execution.open", "test-1", "BUY", "target_usd=1")
	report.Evidence(WireMessage{Subject: "execution.open.result", Payload: []byte(`{"status":"SUCCEEDED"}`)})
	if err := report.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	contents, err := os.ReadFile(report.Path)
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	for _, want := range []string{"# Execution NATS Integration Test", "**PASS**", "filled \\| confirmed", "strategy.execution.open", "execution.open.result", "PASS: 1"} {
		if !strings.Contains(string(contents), want) {
			t.Errorf("report does not contain %q:\n%s", want, contents)
		}
	}
}
