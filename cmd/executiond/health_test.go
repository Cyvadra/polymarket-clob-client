package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestHealthReportsEachCheck(t *testing.T) {
	m := newMetrics()
	get := func() (int, healthReport) {
		rec := httptest.NewRecorder()
		m.serveHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var r healthReport
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return rec.Code, r
	}
	if code, _ := get(); code != http.StatusServiceUnavailable {
		t.Fatalf("before modules start: %d, want 503", code)
	}

	natsUp := true
	reconciled := time.Time{}
	m.lastSuccess.watch("reconcile", func() time.Time { return reconciled })
	m.watchLiveness(liveness{
		started:   time.Now().Add(-time.Hour),
		connected: map[string]func() bool{"nats": func() bool { return natsUp }, "account_stream": func() bool { return false }},
		required:  map[string]bool{"nats": true},
		fresh:     map[string]time.Duration{"reconcile": time.Minute},
	})
	code, r := get()
	if code != http.StatusServiceUnavailable || r.Checks["reconcile"].OK || !r.Checks["nats"].OK {
		t.Fatalf("stale reconcile: %d %+v", code, r)
	}
	reconciled = time.Now()
	if code, r := get(); code != http.StatusOK || !r.OK {
		t.Fatalf("all healthy: %d %+v", code, r)
	}
	natsUp = false
	if code, r := get(); code != http.StatusServiceUnavailable || r.Checks["nats"].OK {
		t.Fatalf("nats down: %d %+v", code, r)
	}
	if err := testutil.GatherAndCompare(m.registry, strings.NewReader(`
# HELP executiond_nats_connected 1 while the nats connection is up.
# TYPE executiond_nats_connected gauge
executiond_nats_connected 0
`), "executiond_nats_connected"); err != nil {
		t.Fatal(err)
	}
	if n, err := testutil.GatherAndCount(m.registry, "executiond_last_success_timestamp_seconds"); err != nil || n != 1 {
		t.Fatalf("last success series: %d %v", n, err)
	}
}
