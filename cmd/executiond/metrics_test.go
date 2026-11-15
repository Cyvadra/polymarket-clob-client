package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
	"github.com/Cyvadra/polymarket-clob-client/pkg/settlement"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

func TestMetricsServeEveryHookSeries(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newMetrics()
	if err := serveMetrics(ctx, addr, m); err != nil {
		t.Fatal(err)
	}

	m.observeOrderHTTP(transport.Timing{Method: "POST", Path: "/orders", FirstByte: 90 * time.Millisecond, Total: 100 * time.Millisecond})
	m.observeOrderHTTP(transport.Timing{Method: "DELETE", Path: "/orders", Reused: true, Total: time.Second, Err: &clobclient.APIError{StatusCode: 503}})
	m.observePrePost(protocol.ExecutionIntent{Kind: protocol.IntentOpen, TimeInForce: protocol.TimeInForceGTC, PostOnly: true}, 3*time.Millisecond)
	m.observeCancel(3, 70*time.Millisecond, nil)
	m.observeCancel(1, 500*time.Millisecond, &clobclient.CancelRefusedError{Reason: "matched"})
	m.observePass("deadline", 1200*time.Millisecond, true)
	m.observeKeepWarm(5*time.Millisecond, 6*time.Millisecond, nil)
	m.observeKeepWarm(0, 0, errors.New("down"))
	m.errorHandler("reconcile", "reconciliation error")(errors.New("boom"))
	m.countError("warm_fees")
	m.observeSweep(settlement.Sweep{Checked: 4, Lost: 1, Redeemed: 2}, time.Unix(1700000000, 0))
	m.observeSweep(settlement.Sweep{Failed: true, Aborted: true}, time.Unix(1700000100, 0))
	m.observeEquity("123.45", time.Unix(1700000000, 0))
	m.observeSizingBase(100)
	guard, err := equity.NewStrategyGroupDrawdownGuard(fakeEquityStore{}, "pmm", time.Unix(0, 0), equity.FractionLimit(0.2))
	if err != nil {
		t.Fatal(err)
	}
	m.observeGuard(guard)

	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		`executiond_order_http_seconds_count{method="POST",outcome="ok",path="/orders"} 1`,
		`executiond_order_http_seconds_count{method="DELETE",outcome="5xx",path="/orders"} 1`,
		`executiond_order_http_cold_connections_total{method="POST",path="/orders"} 1`,
		`executiond_order_http_ttfb_seconds_count{method="POST",path="/orders"} 1`,
		`executiond_order_prepost_seconds_count{kind="OPEN",post_only="true",tif="GTC"} 1`,
		`executiond_cancel_seconds_count{batch="true",outcome="ok"} 1`,
		`executiond_cancel_seconds_count{batch="false",outcome="refused"} 1`,
		`executiond_cancel_batch_orders_sum 4`,
		`executiond_loop_pass_seconds_count{loop="deadline"} 1`,
		`executiond_loop_pass_overruns_total{loop="deadline"} 1`,
		`executiond_keepwarm_rtt_seconds_count{pool="order"} 1`,
		`executiond_keepwarm_failures_total 1`,
		`executiond_errors_total{component="reconcile"} 1`,
		`executiond_errors_total{component="warm_fees"} 1`,
		`executiond_errors_total{component="sweep"} 0`,
		`executiond_sweep_lanes_total{result="redeemed"} 2`,
		`executiond_sweeps_total{outcome="failed"} 1`,
		`executiond_last_success_timestamp_seconds{component="sweep"} 1.7e+09`,
		`executiond_equity_usd 123.45`,
		`executiond_sizing_base_usd 100`,
		`executiond_drawdown_limit{guard="pmm",unit="fraction"} 0.2`,
		`executiond_opens_suspended{guard="pmm"} 0`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %s", want)
		}
	}
}

// fakeEquityStore satisfies store.EquityStore; observeGuard never reads it.
type fakeEquityStore struct{ store.EquityStore }

func TestSweepWhereEveryLaneErroredIsNotASuccess(t *testing.T) {
	m := newMetrics()
	m.observeSweep(settlement.Sweep{Checked: 3, Errored: 3, Failed: true}, time.Unix(100, 0))
	if got := m.lastSuccess.get("sweep"); !got.IsZero() {
		t.Fatalf("all lanes errored, want no success, got %v", got)
	}
	m.observeSweep(settlement.Sweep{Checked: 3, Errored: 1, Failed: true}, time.Unix(200, 0))
	if got := m.lastSuccess.get("sweep"); !got.Equal(time.Unix(200, 0)) {
		t.Fatalf("a partial pass is a success, got %v", got)
	}
}
