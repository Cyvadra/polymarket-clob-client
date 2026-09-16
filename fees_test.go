package clobclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

var cryptoFees = FeeSchedule{Rate: "0.07", Exponent: "1", TakerOnly: true}

// The expected values are the fees the wallet was actually charged: USDC paid
// minus shares * price, from the account activity of 2026-09-15..16.
func TestFeeMatchesWhatTheWalletPaid(t *testing.T) {
	cases := []struct{ shares, price, want string }{
		{"6.28", "0.73", "0.08664"},
		{"9.07", "0.42", "0.15466"},
		{"8.9", "0.5", "0.15575"},
		{"7.31", "0.65", "0.11641"},
	}
	for _, c := range cases {
		got, err := cryptoFees.Fee(c.shares, c.price, true)
		if err != nil || got != c.want {
			t.Fatalf("Fee(%s, %s) = %q, %v; want %s", c.shares, c.price, got, err, c.want)
		}
	}
}

func TestFeeSparesMakersAndUnfeedMarkets(t *testing.T) {
	if got, err := cryptoFees.Fee("7.1", "0.69", false); err != nil || got != "0" {
		t.Fatalf("maker fee = %q, %v", got, err)
	}
	if got, err := (FeeSchedule{}).Fee("7.1", "0.69", true); err != nil || got != "0" {
		t.Fatalf("no-schedule fee = %q, %v", got, err)
	}
	both := FeeSchedule{Rate: "0.07", Exponent: "1"}
	if got, err := both.Fee("6.28", "0.73", false); err != nil || got != "0.08664" {
		t.Fatalf("maker fee without taker_only = %q, %v", got, err)
	}
}

func TestFeeBelowPrecisionIsZero(t *testing.T) {
	if got, err := cryptoFees.Fee("0.01", "0.999", true); err != nil || got != "0.00000" {
		t.Fatalf("tiny fee = %q, %v", got, err)
	}
}

func TestFeeExponent(t *testing.T) {
	squared := FeeSchedule{Rate: "0.25", Exponent: "2", TakerOnly: true}
	// 0.25 * 10 * (0.5*0.5)^2 = 0.15625
	if got, err := squared.Fee("10", "0.5", true); err != nil || got != "0.15625" {
		t.Fatalf("squared fee = %q, %v", got, err)
	}
}

func TestFeeRejectsImpossiblePrices(t *testing.T) {
	for _, price := range []string{"0", "1", "-0.1", "x"} {
		if _, err := cryptoFees.Fee("1", price, true); err == nil {
			t.Fatalf("price %q accepted", price)
		}
	}
}

func TestFeeScheduleIsFetchedOnceAndDecoded(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/clob-markets/0xfee":
			_, _ = w.Write([]byte(`{"c":"0xfee","mts":0.001,"fd":{"r":0.07,"e":1,"to":true}}`))
		case "/clob-markets/0xfree":
			_, _ = w.Write([]byte(`{"c":"0xfree","mts":0.01}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	ctx := context.Background()
	for range 2 {
		got, err := client.FeeSchedule(ctx, "0xfee")
		if err != nil || got != cryptoFees {
			t.Fatalf("schedule = %+v, %v", got, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fetched %d times, want 1", calls.Load())
	}
	free, err := client.FeeSchedule(ctx, "0xfree")
	if err != nil || free != (FeeSchedule{}) {
		t.Fatalf("free schedule = %+v, %v", free, err)
	}
	if _, err := client.FeeSchedule(ctx, " "); err == nil {
		t.Fatal("empty condition accepted")
	}
}
