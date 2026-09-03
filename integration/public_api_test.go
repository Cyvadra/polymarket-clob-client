//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

func integrationClient(t *testing.T) *clobclient.Client {
	t.Helper()
	client, err := clobclient.New(clobclient.Config{HTTPClient: integrationClientHTTP(t)})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func integrationClientHTTP(t *testing.T) *http.Client {
	t.Helper()
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if rawProxy := os.Getenv("CLOB_TEST_PROXY"); rawProxy != "" {
		proxyURL, err := url.Parse(rawProxy)
		if err != nil {
			t.Fatal(err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: transport, Timeout: 20 * time.Second}
}

func TestServerTime(t *testing.T) {
	client := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serverTime, err := client.ServerTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if serverTime < 1_700_000_000 {
		t.Fatalf("implausible server time %d", serverTime)
	}
}

func TestMarkets(t *testing.T) {
	client := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	markets, _, err := client.SamplingMarkets(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(markets) == 0 {
		t.Fatal("CLOB returned no markets")
	}
	if len(markets[0].Tokens) == 0 || markets[0].Tokens[0].TokenID == "" {
		t.Fatal("CLOB market has no token")
	}
	if _, err := client.TickSize(ctx, markets[0].Tokens[0].TokenID); err != nil {
		t.Fatalf("read token tick size: %v", err)
	}
	if _, err := client.OrderBook(ctx, markets[0].Tokens[0].TokenID); err != nil {
		t.Fatalf("read order book: %v", err)
	}
	if _, err := client.OrderBooks(ctx, []string{markets[0].Tokens[0].TokenID}); err != nil {
		t.Fatalf("read batch order books: %v", err)
	}
	if _, err := client.Midpoint(ctx, markets[0].Tokens[0].TokenID); err != nil {
		t.Fatalf("read midpoint: %v", err)
	}
	if _, err := client.Price(ctx, markets[0].Tokens[0].TokenID, clobclient.SideBuy); err != nil {
		t.Fatalf("read price: %v", err)
	}
	if _, err := client.LastTradePrice(ctx, markets[0].Tokens[0].TokenID); err != nil {
		t.Fatalf("read last trade price: %v", err)
	}
	if _, err := client.PriceHistory(ctx, markets[0].Tokens[0].TokenID, clobclient.PriceHistoryOptions{Fidelity: 60, Interval: "1d"}); err != nil {
		t.Fatalf("read price history: %v", err)
	}
	if _, err := client.NegRisk(ctx, markets[0].Tokens[0].TokenID); err != nil {
		t.Fatalf("read neg-risk routing: %v", err)
	}
	if _, err := client.FeeRateBps(ctx, markets[0].Tokens[0].TokenID); err != nil {
		t.Fatalf("read fee rate: %v", err)
	}
	if _, err := client.Market(ctx, markets[0].ConditionID); err != nil {
		t.Fatalf("read individual market: %v", err)
	}
}
