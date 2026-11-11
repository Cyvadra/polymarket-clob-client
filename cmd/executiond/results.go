package main

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
)

// resultMetrics counts what executiond tells strategies: every open, close
// and quote result, and every order lifecycle event, as it is published.
type resultMetrics struct {
	results     *prometheus.CounterVec
	orderEvents *prometheus.CounterVec
	fills       prometheus.Counter
}

func newResultMetrics(registry *prometheus.Registry) *resultMetrics {
	r := &resultMetrics{
		results: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "executiond_results_total", Help: "Open, close and quote results published, by strategy, status and reason code.",
		}, []string{"kind", "strategy", "status", "reason_code"}),
		orderEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "executiond_order_events_total", Help: "Order lifecycle events published, by strategy and state.",
		}, []string{"strategy", "state"}),
		fills: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "executiond_fills_total", Help: "Fills newly recorded from the account stream or reconciliation.",
		}),
	}
	registry.MustRegister(r.results, r.orderEvents, r.fills)
	return r
}

// countingPublisher counts each message before passing it on.
type countingPublisher struct {
	next    protocol.ExecutionEventPublisher
	metrics *resultMetrics
}

func (p countingPublisher) PublishJSON(subject string, payload any) error {
	p.metrics.observe(payload)
	return p.next.PublishJSON(subject, payload)
}

func (r *resultMetrics) observe(payload any) {
	switch m := payload.(type) {
	case protocol.ExecutionOpenResult:
		r.results.WithLabelValues("open", m.Strategy, string(m.Status), m.ReasonCode).Inc()
	case *protocol.ExecutionOpenResult:
		r.observe(*m)
	case protocol.ExecutionCloseResult:
		r.results.WithLabelValues("close", m.Strategy, string(m.Status), m.ReasonCode).Inc()
	case *protocol.ExecutionCloseResult:
		r.observe(*m)
	case protocol.ExecutionQuoteResult:
		r.results.WithLabelValues("quote", m.Strategy, m.Status, "").Inc()
	case *protocol.ExecutionQuoteResult:
		r.observe(*m)
	case protocol.ExecutionOrderEvent:
		r.orderEvents.WithLabelValues(m.Strategy, m.State).Inc()
	case *protocol.ExecutionOrderEvent:
		r.observe(*m)
	}
}
