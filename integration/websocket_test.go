//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	clobws "github.com/Cyvadra/polymarket-clob-client/websocket"
	"github.com/gorilla/websocket"
)

func TestMarketWebSocket(t *testing.T) {
	client := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	markets, _, err := client.SamplingMarkets(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(markets) == 0 || len(markets[0].Tokens) == 0 {
		t.Fatal("no market token available for websocket test")
	}
	dialer := &websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	if rawProxy := os.Getenv("CLOB_TEST_PROXY"); rawProxy != "" {
		proxyURL, err := url.Parse(rawProxy)
		if err != nil {
			t.Fatal(err)
		}
		dialer.Proxy = http.ProxyURL(proxyURL)
	}
	subscription := clobws.MarketSubscription(markets[0].Tokens[0].TokenID)
	subscription["initial_dump"] = true
	stream := clobws.New(clobws.Config{URL: clobws.DefaultMarketURL, Dialer: dialer, StaleTimeout: 20 * time.Second}, subscription)
	go stream.Run(ctx)
	select {
	case event := <-stream.Events():
		if len(event.Data) == 0 {
			t.Fatal("received empty websocket event")
		}
	case err := <-stream.Errors():
		t.Fatalf("websocket error: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for market websocket event")
	}
}
