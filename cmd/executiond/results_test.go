package main

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
)

type nopPublisher struct{ subjects []string }

func (p *nopPublisher) PublishJSON(subject string, _ any) error {
	p.subjects = append(p.subjects, subject)
	return nil
}

func TestCountingPublisherCountsResultsAndPassesThrough(t *testing.T) {
	registry := prometheus.NewRegistry()
	next := &nopPublisher{}
	p := countingPublisher{next: next, metrics: newResultMetrics(registry)}
	_ = p.PublishJSON("a", protocol.ExecutionOpenResult{Strategy: "pmm", Status: protocol.ResultFailed, ReasonCode: protocol.ReasonDrawdownLimit})
	_ = p.PublishJSON("b", &protocol.ExecutionCloseResult{Strategy: "pmm", Status: protocol.ResultSucceeded})
	_ = p.PublishJSON("c", protocol.ExecutionOrderEvent{Strategy: "pmm", State: "FILLED"})
	_ = p.PublishJSON("d", map[string]string{"other": "message"})
	if len(next.subjects) != 4 {
		t.Fatalf("passed on %d of 4", len(next.subjects))
	}
	if err := testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP executiond_results_total Open, close and quote results published, by strategy, status and reason code.
# TYPE executiond_results_total counter
executiond_results_total{kind="close",reason_code="",status="SUCCEEDED",strategy="pmm"} 1
executiond_results_total{kind="open",reason_code="DRAWDOWN_LIMIT",status="FAILED",strategy="pmm"} 1
# HELP executiond_order_events_total Order lifecycle events published, by strategy and state.
# TYPE executiond_order_events_total counter
executiond_order_events_total{state="FILLED",strategy="pmm"} 1
`), "executiond_results_total", "executiond_order_events_total"); err != nil {
		t.Fatal(err)
	}
}
