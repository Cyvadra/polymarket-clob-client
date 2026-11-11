package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// lastSuccess is executiond_last_success_timestamp_seconds: stamps set by
// executiond, plus components that keep their own (the reconciler, the
// account stream) read when scraped.
type lastSuccess struct {
	desc *prometheus.Desc

	mu      sync.Mutex
	stamps  map[string]time.Time
	sources map[string]func() time.Time
}

func newLastSuccess() *lastSuccess {
	return &lastSuccess{
		desc: prometheus.NewDesc("executiond_last_success_timestamp_seconds",
			"Unix time of each component's last successful pass, for staleness alerts.", []string{"component"}, nil),
		stamps: map[string]time.Time{}, sources: map[string]func() time.Time{},
	}
}

func (l *lastSuccess) set(component string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stamps[component] = at
}

// watch reads component's last success from source when scraped.
func (l *lastSuccess) watch(component string, source func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sources[component] = source
}

// get is component's last success, zero if none yet.
func (l *lastSuccess) get(component string) time.Time {
	l.mu.Lock()
	source, ok := l.sources[component]
	at := l.stamps[component]
	l.mu.Unlock()
	if ok {
		return source()
	}
	return at
}

func (l *lastSuccess) Describe(ch chan<- *prometheus.Desc) { ch <- l.desc }

func (l *lastSuccess) Collect(ch chan<- prometheus.Metric) {
	l.mu.Lock()
	names := make([]string, 0, len(l.stamps)+len(l.sources))
	for name := range l.stamps {
		names = append(names, name)
	}
	for name := range l.sources {
		if _, dup := l.stamps[name]; !dup {
			names = append(names, name)
		}
	}
	l.mu.Unlock()
	for _, name := range names {
		if at := l.get(name); !at.IsZero() {
			ch <- prometheus.MustNewConstMetric(l.desc, prometheus.GaugeValue, float64(at.Unix()), name)
		}
	}
}

// liveness is what /healthz and the connection gauges read.
type liveness struct {
	started time.Time
	// connected checks, by name: nats, account_stream.
	connected map[string]func() bool
	// fresh checks: each component must have succeeded, or the daemon have
	// started, within its allowance.
	fresh map[string]time.Duration
}

type healthCheck struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type healthReport struct {
	OK     bool                   `json:"ok"`
	Checks map[string]healthCheck `json:"checks"`
}

// watchLiveness exports a 0/1 executiond_<name>_connected gauge for each
// connection and turns /healthz on.
func (m *metrics) watchLiveness(l liveness) {
	for name, up := range l.connected {
		up := up
		m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "executiond_" + name + "_connected", Help: "1 while the " + name + " connection is up.",
		}, func() float64 {
			if up() {
				return 1
			}
			return 0
		}))
	}
	report := func(now time.Time) healthReport {
		r := healthReport{OK: true, Checks: map[string]healthCheck{}}
		for name, up := range l.connected {
			c := healthCheck{OK: up(), Detail: "connected"}
			if !c.OK {
				c.Detail = "disconnected"
			}
			r.Checks[name] = c
		}
		for name, allowance := range l.fresh {
			last := m.lastSuccess.get(name)
			since := l.started
			detail := "no success since start"
			if last.After(since) {
				since = last
				detail = "last success " + last.UTC().Format(time.RFC3339)
			}
			age := now.Sub(since)
			r.Checks[name] = healthCheck{OK: age <= allowance, Detail: fmt.Sprintf("%s, %s ago (allowed %s)", detail, age.Round(time.Second), allowance)}
		}
		for _, c := range r.Checks {
			r.OK = r.OK && c.OK
		}
		return r
	}
	m.health.Store(&report)
}

// serveHealth answers 200 when every check passes and 503 otherwise, with the
// checks as JSON either way.
func (m *metrics) serveHealth(w http.ResponseWriter, _ *http.Request) {
	report := healthReport{Checks: map[string]healthCheck{"startup": {Detail: "modules not started"}}}
	if fn := m.health.Load(); fn != nil {
		report = (*fn)(time.Now())
	}
	w.Header().Set("Content-Type", "application/json")
	if !report.OK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(report)
}
