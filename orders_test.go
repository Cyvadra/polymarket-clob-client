package clobclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMarketSellOrderUsesWorstRequiredBid(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/book":
			_, _ = w.Write([]byte(`{"bids":[{"price":"0.55","size":"2"},{"price":"0.49","size":"4"}]}`))
		case "/tick-size":
			_, _ = w.Write([]byte(`{"minimum_tick_size":0.01}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := New(Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	order, err := client.MarketSellOrder(context.Background(), "token", 5, OrderTypeFOK)
	if err != nil {
		t.Fatal(err)
	}
	if order.Price != .49 || order.Side != SideSell {
		t.Fatalf("unexpected market sell order %+v", order)
	}
}

func TestMarketBuyOrderUsesWorstRequiredAsk(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/book":
			_, _ = w.Write([]byte(`{"asks":[{"price":"0.40","size":"2"},{"price":"0.50","size":"4"}]}`))
		case "/tick-size":
			_, _ = w.Write([]byte(`{"minimum_tick_size":0.01}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := New(Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	order, err := client.MarketBuyOrder(context.Background(), "token", 2, OrderTypeFOK)
	if err != nil {
		t.Fatal(err)
	}
	if order.Price != .50 || order.Shares != 4 || order.Side != SideBuy {
		t.Fatalf("unexpected market buy order %+v", order)
	}
}

func TestMarketOrdersSortLevelsAndEnforceMinimumSize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/book":
			_, _ = w.Write([]byte(`{"bids":[{"price":"0.40","size":"3"},{"price":"0.50","size":"3"}],"asks":[{"price":"0.60","size":"3"},{"price":"0.50","size":"3"}],"min_order_size":"5"}`))
		case "/tick-size":
			_, _ = w.Write([]byte(`{"minimum_tick_size":0.01}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := New(Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.MarketSellOrder(context.Background(), "token", 4, OrderTypeFOK); err == nil {
		t.Fatal("expected minimum-size rejection")
	}
	order, err := client.MarketSellOrder(context.Background(), "token", 5, OrderTypeFOK)
	if err != nil || order.Price != .4 {
		t.Fatalf("unexpected sorted sell: order=%+v err=%v", order, err)
	}
}

func TestInvalidateMarketMetadataRefreshesToken(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tick-size" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		calls++
		_, _ = w.Write([]byte(`{"minimum_tick_size":0.01}`))
	}))
	defer server.Close()
	client, err := New(Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.TickSize(context.Background(), "token"); err != nil {
		t.Fatal(err)
	}
	client.InvalidateMarketMetadata("token")
	if _, err := client.TickSize(context.Background(), "token"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("tick-size calls=%d, want 2", calls)
	}
}
