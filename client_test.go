package clobclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testKey = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

func newTestClient(t *testing.T, host string) *Client {
	t.Helper()
	client, err := New(Config{
		Host: host, ChainID: ChainPolygonAmoy, PrivateKey: testKey,
		Credentials: &Credentials{APIKey: "key", Secret: base64.StdEncoding.EncodeToString([]byte("secret")), Passphrase: "pass"},
		Now:         func() time.Time { return time.Unix(1_710_000_000, 123_000_000) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestNewRejectsNegativeRetryAttempts(t *testing.T) {
	if _, err := New(Config{Retry: RetryConfig{MaxAttempts: -1}}); err == nil {
		t.Fatal("expected invalid retry configuration")
	}
}

func TestSubmitOrderUsesV2PayloadAndCanonicalAuthPath(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tick-size":
			_, _ = io.WriteString(w, `{"minimum_tick_size":0.01}`)
		case "/neg-risk":
			_, _ = io.WriteString(w, `{"neg_risk":true}`)
		case "/order":
			if got := r.Header.Get("POLY_API_KEY"); got != "key" {
				t.Fatalf("api key = %q", got)
			}
			if r.Header.Get("POLY_SIGNATURE") == "" {
				t.Fatal("missing L2 signature")
			}
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &captured); err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(w, `{"success":true,"orderID":"o-1","status":"live"}`)
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	response, err := client.SubmitOrder(context.Background(), UserOrder{TokenID: "1234", Side: SideBuy, Price: 0.5, Shares: 10, OrderType: OrderTypeGTC})
	if err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if response.OrderID != "o-1" {
		t.Fatalf("order ID = %q", response.OrderID)
	}
	order := captured["order"].(map[string]any)
	for _, field := range []string{"timestamp", "metadata", "builder", "signature"} {
		if _, ok := order[field]; !ok {
			t.Errorf("missing V2 field %s", field)
		}
	}
	for _, field := range []string{"nonce", "taker", "feeRateBps", "orderID"} {
		if _, ok := order[field]; ok {
			t.Errorf("legacy field %s found in V2 payload", field)
		}
	}
}

func TestSubmitSignedOrderAvoidsResigning(t *testing.T) {
	var received SignedOrderV2
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tick-size":
			_, _ = io.WriteString(w, `{"minimum_tick_size":0.01}`)
		case "/neg-risk":
			_, _ = io.WriteString(w, `{"neg_risk":false}`)
		case "/order":
			var payload struct {
				Order SignedOrderV2 `json:"order"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			received = payload.Order
			_, _ = io.WriteString(w, `{"success":true,"orderID":"o-1"}`)
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	signed, err := client.CreateOrder(context.Background(), UserOrder{TokenID: "1234", Side: SideBuy, Price: .5, Shares: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SubmitSignedOrder(context.Background(), signed, OrderTypeGTC, false); err != nil {
		t.Fatal(err)
	}
	if signed.OrderID == "" {
		t.Fatal("expected signed order ID")
	}
	if received.Signature != signed.Signature || received.Salt != signed.Salt {
		t.Fatalf("submitted order was changed: got=%+v want=%+v", received, signed)
	}
}

func TestAllOpenOrdersAndTradesFollowCursors(t *testing.T) {
	var orderCalls, tradeCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/data/orders":
			orderCalls++
			if r.URL.Query().Get("next_cursor") == "next-orders" {
				_, _ = io.WriteString(w, `{"data":[{"id":"o-2"}],"next_cursor":""}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"o-1"}],"next_cursor":"next-orders"}`)
		case "/data/trades":
			tradeCalls++
			if r.URL.Query().Get("next_cursor") == "next-trades" {
				_, _ = io.WriteString(w, `{"data":[{"id":"t-2","taker_order_id":"o-2","outcome":"Down","trader_side":"TAKER"}],"next_cursor":""}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"t-1","taker_order_id":"o-1","outcome":"Up","trader_side":"MAKER","maker_orders":[{"order_id":"m-1","owner":"key","matched_amount":"1","asset_id":"token","outcome":"Up","side":"SELL"}]}],"next_cursor":"next-trades"}`)
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	orders, err := client.AllOpenOrders(context.Background())
	if err != nil || len(orders) != 2 || orderCalls != 2 {
		t.Fatalf("open-order pagination: orders=%+v calls=%d err=%v", orders, orderCalls, err)
	}
	trades, err := client.AllTrades(context.Background())
	if err != nil || len(trades) != 2 || tradeCalls != 2 || trades[0].MakerOrders[0].OrderID != "m-1" || trades[1].TakerOrderID != "o-2" {
		t.Fatalf("trade pagination: trades=%+v calls=%d err=%v", trades, tradeCalls, err)
	}
}

func TestTransportEscapesAndSignsCanonicalQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.RawQuery, "asset_type=COLLATERAL&signature_type=0&token_id=a%2Fb%3Fc"; got != want {
			t.Fatalf("query = %q, want %q", got, want)
		}
		if r.Header.Get("POLY_SIGNATURE") == "" {
			t.Fatal("missing signature")
		}
		_, _ = io.WriteString(w, `{"balance":"0","allowance":"0"}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	_, err := client.BalanceAllowance(context.Background(), "COLLATERAL", "a/b?c")
	if err != nil {
		t.Fatal(err)
	}
}

func TestHMACSignatureUsesDecodedSecret(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("secret"))
	got, err := hmacSignature(secret, 1, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got == "" || strings.Contains(got, "=") == false {
		t.Fatal("expected padded URL base64 signature")
	}
}

func TestOrderAmountsMarketBuyUsesTwoDecimalUSDC(t *testing.T) {
	maker, taker := orderAmounts(SideBuy, .43, 6.04, OrderTypeFOK)
	if new(big.Int).Mod(maker, big.NewInt(10_000)).Sign() != 0 {
		t.Fatalf("market buy maker amount %s is not cents", maker)
	}
	if new(big.Int).Mod(taker, big.NewInt(100)).Sign() != 0 {
		t.Fatalf("market buy taker amount %s is not four decimals", taker)
	}
}

func TestOrderAmountsAvoidFloatArtifactAtMicroUnitBoundary(t *testing.T) {
	maker, taker := orderAmounts(SideBuy, .29, 3.45, OrderTypeGTC)
	if maker.String() != "1000500" || taker.String() != "3450000" {
		t.Fatalf("unexpected amounts maker=%s taker=%s", maker, taker)
	}
}

func TestOrderAmountsDoNotOverflowInt64(t *testing.T) {
	maker, taker := orderAmounts(SideSell, .5, 10_000_000_000_000, OrderTypeGTC)
	if maker.Cmp(big.NewInt(0).SetUint64(10_000_000_000_000_000_000)) != 0 || taker.String() != "5000000000000000000" {
		t.Fatalf("unexpected large amounts maker=%s taker=%s", maker, taker)
	}
}

func TestAPIErrorExposesStatusAndPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()
	client, err := New(Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ServerTime(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest || apiErr.Path != "/time" {
		t.Fatalf("unexpected API error: %#v", err)
	}
}

func TestNewPreservesExplicitSignatureType(t *testing.T) {
	client, err := New(Config{
		PrivateKey:    testKey,
		MakerAddress:  "0x0000000000000000000000000000000000000001",
		SignatureType: SignatureTypeEOA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.cfg.SignatureType != SignatureTypeEOA {
		t.Fatalf("signature type changed to %d", client.cfg.SignatureType)
	}
}

func TestEnsureCredentialsDerivesOnceAndFallsBackToCreate(t *testing.T) {
	var paths []string
	deriveFails := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/auth/derive-api-key" && deriveFails {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"apiKey":"derived","secret":"c2VjcmV0","passphrase":"phrase"}`))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	client.cfg.Credentials = nil
	client.cfg.Retry.MaxAttempts = 1
	credentials, err := client.EnsureCredentials(context.Background())
	if err != nil {
		t.Fatalf("EnsureCredentials: %v", err)
	}
	if credentials.APIKey != "derived" {
		t.Fatalf("api key = %q", credentials.APIKey)
	}
	want := []string{"GET /auth/derive-api-key", "POST /auth/api-key"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}

	deriveFails = false
	if _, err := client.EnsureCredentials(context.Background()); err != nil {
		t.Fatalf("EnsureCredentials again: %v", err)
	}
	if len(paths) != len(want) {
		t.Fatalf("second call issued requests: %v", paths)
	}
	if client.cfg.Credentials == nil || client.cfg.Credentials.APIKey != "derived" {
		t.Fatalf("credentials not cached on client")
	}
}

func TestPaginationStopsAtEndCursorSentinel(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Path {
		case "/data/orders":
			_, _ = io.WriteString(w, `{"data":[{"id":"o-1"}],"next_cursor":"LTE="}`)
		case "/data/trades":
			_, _ = io.WriteString(w, `{"data":[{"id":"t-1"}],"next_cursor":"LTE="}`)
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	orders, err := client.AllOpenOrders(context.Background())
	if err != nil || len(orders) != 1 {
		t.Fatalf("open orders = %+v, err = %v", orders, err)
	}
	trades, err := client.AllTrades(context.Background())
	if err != nil || len(trades) != 1 {
		t.Fatalf("trades = %+v, err = %v", trades, err)
	}
	want := []string{"/data/orders?", "/data/trades?"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if _, next, err := client.Trades(context.Background(), "LTE="); err != nil || next != "LTE=" || len(calls) != 2 {
		t.Fatalf("explicit end cursor issued a request: next=%q err=%v calls=%v", next, err, calls)
	}
}

func TestOrderLookupTreatsEmptyBodyAsNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if _, err := newTestClient(t, server.URL).Order(context.Background(), "0xabc"); err == nil {
		t.Fatal("expected lookup error")
	} else {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
			t.Fatalf("Order error = %v", err)
		}
	}
}
