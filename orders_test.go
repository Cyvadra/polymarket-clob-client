package clobclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
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

// After WarmMarketMetadata, signing the first order on a token must not touch
// the network: that round trip is what delayed live entries.
func TestWarmedTokenSignsWithoutNetwork(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/book":
			_, _ = w.Write([]byte(`{"min_order_size":"5","asks":[{"price":"0.40","size":"20"}]}`))
		case "/tick-size":
			_, _ = w.Write([]byte(`{"minimum_tick_size":0.01}`))
		case "/neg-risk":
			_, _ = w.Write([]byte(`{"neg_risk":false}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	ctx := context.Background()
	if err := client.WarmMarketMetadata(ctx, "token"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	warmed := len(calls)
	for path, n := range calls {
		if n != 1 {
			t.Fatalf("%s fetched %d times while warming", path, n)
		}
	}
	mu.Unlock()
	if warmed != 3 {
		t.Fatalf("warmed %d endpoints, want 3: %v", warmed, calls)
	}
	if _, err := client.CreateOrder(ctx, UserOrder{TokenID: "token", Side: SideBuy, Price: 0.4, Shares: 12.25}); err != nil {
		t.Fatal(err)
	}
	if size, err := client.MinOrderSize(ctx, "token"); err != nil || size != 5 {
		t.Fatalf("min order size = %v, %v", size, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for path, n := range calls {
		if n != 1 {
			t.Fatalf("%s fetched again after warming (%d calls)", path, n)
		}
	}
}

func TestWarmMarketMetadataReportsFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client, err := New(Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WarmMarketMetadata(context.Background(), "token"); err == nil {
		t.Fatal("expected an error when every lookup fails")
	}
	if err := client.WarmMarketMetadata(context.Background(), ""); err == nil {
		t.Fatal("expected an error for an empty token")
	}
}
