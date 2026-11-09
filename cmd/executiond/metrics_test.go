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
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %s", want)
		}
	}
}
