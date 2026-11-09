package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
)

// latencyBuckets span a warm keep-alive round trip (a few ms) to a stalled
// cancel (seconds): the cancel p90 was 533ms and its max 1.4s on 2026-11-09.
var latencyBuckets = []float64{.0025, .005, .01, .025, .05, .075, .1, .15, .2, .3, .5, .75, 1, 1.5, 2.5, 5}

// metrics is what executiond exposes on /metrics. Every series is fed by a
// hook the client or executor already calls, alongside the log lines those
// hooks print.
type metrics struct {
	registry *prometheus.Registry

	orderHTTP     *prometheus.HistogramVec // order pool requests, by method, path, outcome
	orderTTFB     *prometheus.HistogramVec
	orderColdConn *prometheus.CounterVec // requests that paid for a new connection
	prePost       *prometheus.HistogramVec
	cancel        *prometheus.HistogramVec // exchange cancel calls, single or batch
	cancelOrders  prometheus.Histogram     // orders per cancel call
	pass          *prometheus.HistogramVec
	passOverruns  *prometheus.CounterVec
	keepWarm      *prometheus.HistogramVec
	keepWarmFails prometheus.Counter
}

func newMetrics() *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		orderHTTP: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "executiond_order_http_seconds", Help: "Order pool HTTP requests, start to body read.", Buckets: latencyBuckets,
		}, []string{"method", "path", "outcome"}),
		orderTTFB: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "executiond_order_http_ttfb_seconds", Help: "Order pool HTTP requests, start to first response byte.", Buckets: latencyBuckets,
		}, []string{"method", "path"}),
		orderColdConn: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "executiond_order_http_cold_connections_total", Help: "Order pool requests that did not reuse a connection.",
		}, []string{"method", "path"}),
		prePost: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "executiond_order_prepost_seconds", Help: "Request arrival to order POST: sizing, locks, persistence and signing.", Buckets: latencyBuckets,
		}, []string{"kind", "tif", "post_only"}),
		cancel: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "executiond_cancel_seconds", Help: "Exchange cancel calls, from send to answer.", Buckets: latencyBuckets,
		}, []string{"batch", "outcome"}),
		cancelOrders: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "executiond_cancel_batch_orders", Help: "Orders carried by each exchange cancel call.", Buckets: []float64{1, 2, 3, 5, 10, 15},
		}),
		pass: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "executiond_loop_pass_seconds", Help: "Duration of each executor background loop pass.", Buckets: latencyBuckets,
		}, []string{"loop"}),
		passOverruns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "executiond_loop_pass_overruns_total", Help: "Loop passes that took over ten intervals.",
		}, []string{"loop"}),
		keepWarm: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "executiond_keepwarm_rtt_seconds", Help: "Keep-warm ping round trips, by connection pool.", Buckets: latencyBuckets,
		}, []string{"pool"}),
		keepWarmFails: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "executiond_keepwarm_failures_total", Help: "Keep-warm pings that failed.",
		}),
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.orderHTTP, m.orderTTFB, m.orderColdConn, m.prePost, m.cancel, m.cancelOrders, m.pass, m.passOverruns, m.keepWarm, m.keepWarmFails,
	)
	return m
}

// observeOrderHTTP takes the client's request trace: every order POST, batch
// POST, cancel and keep-warm ping on the order pool.
func (m *metrics) observeOrderHTTP(t transport.Timing) {
	m.orderHTTP.WithLabelValues(t.Method, t.Path, outcome(t.Err)).Observe(t.Total.Seconds())
	if t.FirstByte > 0 {
		m.orderTTFB.WithLabelValues(t.Method, t.Path).Observe(t.FirstByte.Seconds())
	}
	if !t.Reused {
		m.orderColdConn.WithLabelValues(t.Method, t.Path).Inc()
	}
}

func (m *metrics) observePrePost(intent protocol.ExecutionIntent, prePost time.Duration) {
	m.prePost.WithLabelValues(string(intent.Kind), string(intent.TimeInForce), strconv.FormatBool(intent.PostOnly)).Observe(prePost.Seconds())
}

func (m *metrics) observeCancel(orders int, took time.Duration, err error) {
	m.cancel.WithLabelValues(strconv.FormatBool(orders > 1), outcome(err)).Observe(took.Seconds())
	m.cancelOrders.Observe(float64(orders))
}

func (m *metrics) observePass(loop string, took time.Duration, overrun bool) {
	m.pass.WithLabelValues(loop).Observe(took.Seconds())
	if overrun {
		m.passOverruns.WithLabelValues(loop).Inc()
	}
}

func (m *metrics) observeKeepWarm(orders, reads time.Duration, err error) {
	if err != nil {
		m.keepWarmFails.Inc()
		return
	}
	m.keepWarm.WithLabelValues("order").Observe(orders.Seconds())
	m.keepWarm.WithLabelValues("read").Observe(reads.Seconds())
}

// outcome keeps the label set small: ok, a 4xx or 5xx class, a refused
// cancel, or any other error.
func outcome(err error) string {
	if err == nil {
		return "ok"
	}
	var refused *clobclient.CancelRefusedError
	if errors.As(err, &refused) {
		return "refused"
	}
	var apiErr *clobclient.APIError
	if errors.As(err, &apiErr) {
		return strconv.Itoa(apiErr.StatusCode/100) + "xx"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "error"
}

// serveMetrics serves /metrics on addr until ctx ends. An empty addr turns
// it off.
func serveMetrics(ctx context.Context, addr string, m *metrics) error {
	if addr == "" {
		return nil
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics server stopped: %v", err)
		}
	}()
	log.Printf("metrics on http://%s/metrics", listener.Addr())
	return nil
}
