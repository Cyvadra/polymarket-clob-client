package clobclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// CancelOrderResult surfaces the exchange's refusal reason and sends a
// refused or failed cancel once: the caller paces its own retries.
func TestCancelOrderResultReportsRefusalWithoutRetry(t *testing.T) {
	var calls int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if r.Method != http.MethodDelete || r.URL.Path != "/order" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		switch n {
		case 1:
			_, _ = w.Write([]byte(`{"canceled":[],"not_canceled":{"0xabc":"order is pending"}}`))
		case 2:
			_, _ = w.Write([]byte(`{"canceled":["0xABC"],"not_canceled":{}}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)

	refused, err := client.CancelOrderResult(context.Background(), "0xabc")
	if err != nil {
		t.Fatal(err)
	}
	if refused.Has("0xabc") || refused.NotCanceled["0xabc"] != "order is pending" {
		t.Fatalf("unexpected refusal %+v", refused)
	}
	canceled, err := client.CancelOrderResult(context.Background(), "0xabc")
	if err != nil || !canceled.Has("0xabc") {
		t.Fatalf("expected cancel, got %+v %v", canceled, err)
	}
	if _, err := client.CancelOrderResult(context.Background(), "0xabc"); err == nil {
		t.Fatal("expected error for 503")
	}
	if calls != 3 {
		t.Fatalf("expected 3 requests (no retry), got %d", calls)
	}
}

func TestCancelOrderReturnsRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"canceled":[],"not_canceled":{"0xABC":"order can't be found - already canceled or matched"}}`))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CancelOrder(context.Background(), "0xabc")
	var refused *CancelRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("expected a refusal, got %v", err)
	}
	if !refused.OrderGone() {
		t.Fatalf("refusal %q should read as the order already gone", refused.Reason)
	}
	if (&CancelRefusedError{Reason: "order is pending"}).OrderGone() {
		t.Fatal("a pending order is not gone")
	}
}

func TestSubmitSignedOrdersPostsOneBatchAndMapsEachAnswer(t *testing.T) {
	var body []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/orders" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("POLY_SIGNATURE") == "" {
			t.Fatal("missing L2 signature")
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`[{"success":true,"orderID":"0x1","status":"live"},{"success":true,"orderID":"","errorMsg":"invalid post-only order: order crosses book"}]`))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	signed := SignedOrderV2{TokenID: "123", Signature: "0xsig"}

	out, err := client.SubmitSignedOrders(context.Background(), []BatchOrder{{Signed: signed, PostOnly: true}, {Signed: signed, OrderType: OrderTypeGTC, PostOnly: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 || body[0]["owner"] != "key" || body[0]["orderType"] != "GTC" || body[1]["postOnly"] != true {
		t.Fatalf("unexpected payload %+v", body)
	}
	if BatchOrderError(out[0]) != nil || out[0].OrderID != "0x1" {
		t.Fatalf("first order should be accepted: %+v", out[0])
	}
	var rejected *OrderRejectedError
	if !errors.As(BatchOrderError(out[1]), &rejected) || !strings.Contains(rejected.Message, "crosses book") {
		t.Fatalf("second order should be rejected: %+v", out[1])
	}
	if BatchOrderError(OrderResponse{Success: false}) == nil {
		t.Fatal("success=false must read as a rejection")
	}
}

func TestSubmitSignedOrdersRejectsMismatchedAnswerAndOversizedBatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"success":true,"orderID":"0x1"}]`))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	signed := SignedOrderV2{TokenID: "123", Signature: "0xsig"}
	if _, err := client.SubmitSignedOrders(context.Background(), []BatchOrder{{Signed: signed}, {Signed: signed}}); err == nil {
		t.Fatal("expected an error for 1 answer to 2 orders")
	}
	if _, err := client.SubmitSignedOrders(context.Background(), make([]BatchOrder, MaxBatchOrders+1)); err == nil {
		t.Fatal("expected an error for an oversized batch")
	}
}

func TestCancelOrdersResultSendsIDListAndReadsRefusals(t *testing.T) {
	var ids []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/orders" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"canceled":["0xA"],"not_canceled":{"0xb":"order can't be found - already canceled or matched"}}`))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)

	out, err := client.CancelOrdersResult(context.Background(), []string{"0xa", "0xb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "0xa" {
		t.Fatalf("unexpected body %v", ids)
	}
	if !out.Has("0xa") {
		t.Fatal("0xa should be canceled")
	}
	if reason, refused := out.Refused("0xB"); !refused || !(&CancelRefusedError{Reason: reason}).OrderGone() {
		t.Fatalf("0xb should be refused as gone, got %q", reason)
	}
}
