//go:build integration

package integration

import (
	"context"
	"os"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/data"
	"github.com/Cyvadra/polymarket-clob-client/gamma"
)

func TestGammaMarketAndDataPositions(t *testing.T) {
	privateKey := os.Getenv("CLOB_TEST_PRIVATE_KEY")
	if privateKey == "" {
		t.Fatal("CLOB_TEST_PRIVATE_KEY is required for service integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	clob, err := clobclient.New(clobclient.Config{PrivateKey: privateKey, HTTPClient: integrationClientHTTP(t)})
	if err != nil {
		t.Fatal(err)
	}
	markets, _, err := clob.SamplingMarkets(ctx, 1)
	if err != nil || len(markets) == 0 || markets[0].Slug == "" {
		t.Fatalf("sampling market: %v", err)
	}
	gammaClient, err := gamma.New(gamma.Config{HTTPClient: integrationClientHTTP(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gammaClient.MarketBySlug(ctx, markets[0].Slug); err != nil {
		t.Fatalf("Gamma market lookup: %v", err)
	}
	dataClient, err := data.New(data.Config{HTTPClient: integrationClientHTTP(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataClient.Positions(ctx, clob.Address(), 10, 0); err != nil {
		t.Fatalf("Data API positions: %v", err)
	}
}
