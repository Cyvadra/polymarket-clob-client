package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor"
	"github.com/Cyvadra/polymarket-clob-client/pkg/settlement"
)

// latencyBuckets span a warm keep-alive round trip (a few ms) to a stalled
// cancel (seconds): the cancel p90 was 533ms and its max 1.4s on 2026-11-09.
var latencyBuckets = []float64{.001, .002, .003, .005, .0075, .01, .015, .025, .05, .075, .1, .15, .2, .3, .5, .75, 1, 1.5, 2.5, 5}

// metrics is what executiond exposes on /metrics. Every series is fed by a
// hook the client or executor already calls, alongside the log lines those
// hooks print.
type metrics struct {
	registry *prometheus.Registry

	orderHTTP     *prometheus.HistogramVec // order pool requests, by method, path, outcome
	orderTTFB     *prometheus.HistogramVec
	orderColdConn *prometheus.CounterVec // requests that paid for a new connection
	prePost       *prometheus.HistogramVec
	prePostStage  *prometheus.HistogramVec // pre-post latency by stage
	cancel        *prometheus.HistogramVec // exchange cancel calls, single or batch
	cancelOrders  prometheus.Histogram     // orders per cancel call
	pass          *prometheus.HistogramVec
	passOverruns  *prometheus.CounterVec
	keepWarm      *prometheus.HistogramVec
	keepWarmFails prometheus.Counter
	quoteLevels   *prometheus.CounterVec
	quoteFill     prometheus.Histogram
	errors        *prometheus.CounterVec // errors that would otherwise only be logged, by component
	lastSuccess   *lastSuccess
	health        atomic.Pointer[func(time.Time) healthReport]
	equity        prometheus.Gauge
	sizingBase    prometheus.Gauge
	drawdown      *prometheus.GaugeVec
	drawdownLimit *prometheus.GaugeVec
	suspended     *prometheus.GaugeVec
	sweepLanes    *prometheus.CounterVec
	sweeps        *prometheus.CounterVec
}

// errorComponents are the values of executiond_errors_total's component
// label. Each is pre-created at zero, so a rate() alert sees the series
// before its first error.
var errorComponents = []string{
	"nats_handler", "position_publish", "account_stream", "fills", "order_results", "lifecycle",
	"reconcile", "sweep", "refresh_sizing", "cash_flow", "equity_record", "drawdown_refresh",
	"warm_fees", "warm_metadata", "metrics_server", "history", "cash_refresh", "orphan_order",
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
		prePostStage: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "executiond_order_prepost_stage_seconds", Help: "Pre-post latency by stage; stages an order skipped are not observed.", Buckets: latencyBuckets,
		}, []string{"stage"}),
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
		quoteLevels: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "executiond_quote_levels_total", Help: "Quote lane level posts and cancels, by action and reason.",
		}, []string{"action", "reason"}),
		quoteFill: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "executiond_quote_fill_to_cancel_seconds", Help: "A quote lane's first fill seen to the batch cancel of its other levels answered.", Buckets: latencyBuckets,
		}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "executiond_errors_total", Help: "Errors reported by each executiond component; the log line carries the detail.",
		}, []string{"component"}),
	}
	m.lastSuccess = newLastSuccess()
	m.equity = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "executiond_equity_usd", Help: "Wallet equity in the latest recorded snapshot.",
	})
	m.sizingBase = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "executiond_sizing_base_usd", Help: "Equity that equity-fraction opens are sized from; 0 until a base is recorded.",
	})
	m.drawdown = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "executiond_drawdown", Help: "Each guard's drawdown as last read, in its limit's unit.",
	}, []string{"guard", "unit"})
	m.drawdownLimit = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "executiond_drawdown_limit", Help: "Each guard's drawdown limit, in the unit of executiond_drawdown.",
	}, []string{"guard", "unit"})
	m.suspended = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "executiond_opens_suspended", Help: "1 while a guard holds back opens.",
	}, []string{"guard"})
	m.sweepLanes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "executiond_sweep_lanes_total", Help: "Lanes each settlement sweep handled, by result.",
	}, []string{"result"})
	m.sweeps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "executiond_sweeps_total", Help: "Settlement sweep passes, by outcome.",
	}, []string{"outcome"})
	for _, component := range errorComponents {
		m.errors.WithLabelValues(component)
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.orderHTTP, m.orderTTFB, m.orderColdConn, m.prePost, m.prePostStage, m.cancel, m.cancelOrders, m.pass, m.passOverruns, m.keepWarm, m.keepWarmFails,
		m.quoteLevels, m.quoteFill, m.errors,
		m.lastSuccess, m.equity, m.sizingBase, m.drawdown, m.drawdownLimit, m.suspended, m.sweepLanes, m.sweeps,
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

func (m *metrics) observePrePost(intent protocol.ExecutionIntent, prePost time.Duration, s executor.PrePostStages) {
	m.prePost.WithLabelValues(string(intent.Kind), string(intent.TimeInForce), strconv.FormatBool(intent.PostOnly)).Observe(prePost.Seconds())
	for _, st := range []struct {
		name string
		took time.Duration
	}{{"size", s.Size}, {"insert", s.Insert}, {"plan", s.Plan}, {"reserve", s.Reserve}, {"sign", s.Sign}, {"publish", s.Publish}, {"persist_wait", s.PersistWait}} {
		if st.took > 0 {
			m.prePostStage.WithLabelValues(st.name).Observe(st.took.Seconds())
		}
	}
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
	m.succeeded("keepwarm", time.Now())
}

func (m *metrics) observeQuoteLevels(action, reason string, n int) {
	m.quoteLevels.WithLabelValues(action, reason).Add(float64(n))
}

func (m *metrics) observeQuoteFillToCancel(took time.Duration) {
	m.quoteFill.Observe(took.Seconds())
}

// succeeded stamps component's last successful pass.
func (m *metrics) succeeded(component string, at time.Time) {
	m.lastSuccess.set(component, at)
}

func (m *metrics) observeSweep(s settlement.Sweep, at time.Time) {
	for result, n := range map[string]int{
		"checked": s.Checked, "lost": s.Lost, "redeemed": s.Redeemed, "shrunk": s.Shrunk,
		"dust": s.Dust, "closing": s.Reserved, "settling": s.Settling, "errored": s.Errored,
	} {
		m.sweepLanes.WithLabelValues(result).Add(float64(n))
	}
	// A pass that reached every lane is a success for health even if some
	// lanes errored: one lane that keeps failing shows on
	// executiond_errors_total and the "partial" outcome, and must not mark
	// a sweeper that settles every other lane as stale. A pass where every
	// lane errored settled nothing, so it is a failure.
	switch {
	case s.Aborted, s.Checked > 0 && s.Errored == s.Checked:
		m.sweeps.WithLabelValues("failed").Inc()
		return
	case s.Failed:
		m.sweeps.WithLabelValues("partial").Inc()
	default:
		m.sweeps.WithLabelValues("ok").Inc()
	}
	m.succeeded("sweep", at)
}

// observeEquity takes a recorded snapshot's equity, a decimal string.
func (m *metrics) observeEquity(equityUSD string, at time.Time) {
	if v, err := strconv.ParseFloat(equityUSD, 64); err == nil {
		m.equity.Set(v)
		m.succeeded("equity_snapshot", at)
	}
}

func (m *metrics) observeSizingBase(usd float64) { m.sizingBase.Set(usd) }

// observeGuard takes a guard's reading after a refresh.
func (m *metrics) observeGuard(guard *equity.DrawdownGuard) {
	label := guardLabel(guard)
	drawdown, limit, dollars := guard.Reading()
	unit := "fraction"
	if dollars {
		unit = "usd"
	}
	m.drawdown.WithLabelValues(label, unit).Set(drawdown)
	m.drawdownLimit.WithLabelValues(label, unit).Set(limit)
	suspended := 0.0
	if s, _ := guard.Suspended(); s {
		suspended = 1
	}
	m.suspended.WithLabelValues(label).Set(suspended)
}

// countError counts one error from component. The caller still logs it.
func (m *metrics) countError(component string) {
	m.errors.WithLabelValues(component).Inc()
}

// errorHandler returns an error handler that logs as "<label>: <err>" and
// counts the error under component.
func (m *metrics) errorHandler(component, label string) func(error) {
	return func(err error) {
		log.Printf("%s: %v", label, err)
		m.countError(component)
	}
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

// serveMetrics serves /metrics and /healthz on addr until ctx ends. An empty addr turns
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
	mux.HandleFunc("/healthz", m.serveHealth)
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
			m.countError("metrics_server")
		}
	}()
	log.Printf("metrics on http://%s/metrics", listener.Addr())
	return nil
}
