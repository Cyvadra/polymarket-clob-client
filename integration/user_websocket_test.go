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
	clobws "github.com/Cyvadra/polymarket-clob-client/websocket"
	"github.com/gorilla/websocket"
)

func TestUserWebSocketSubscription(t *testing.T) {
	privateKey := os.Getenv("CLOB_TEST_PRIVATE_KEY")
	if privateKey == "" {
		t.Fatal("CLOB_TEST_PRIVATE_KEY is required for authenticated integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bootstrap, err := clobclient.New(clobclient.Config{PrivateKey: privateKey, HTTPClient: integrationClientHTTP(t)})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := bootstrap.DeriveCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	markets, _, err := integrationClient(t).SamplingMarkets(ctx, 1)
	if err != nil || len(markets) == 0 {
		t.Fatalf("sampling markets: %v", err)
	}
	dialer := &websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	if rawProxy := os.Getenv("CLOB_TEST_PROXY"); rawProxy != "" {
		proxyURL, err := url.Parse(rawProxy)
		if err != nil {
			t.Fatal(err)
		}
		dialer.Proxy = http.ProxyURL(proxyURL)
	}
	stream := clobws.New(clobws.Config{URL: clobws.DefaultUserURL, Dialer: dialer}, clobws.UserSubscription([]string{markets[0].ConditionID}, clobws.UserCredentials{APIKey: credentials.APIKey, Secret: credentials.Secret, Passphrase: credentials.Passphrase}))
	go stream.Run(ctx)
	select {
	case <-stream.Ready():
	case err := <-stream.Errors():
		t.Fatalf("user websocket error: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out establishing user websocket")
	}
	select {
	case err := <-stream.Errors():
		t.Fatalf("user websocket rejected subscription: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
}
