//go:build integration

package integration

import (
	"context"
	"os"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

func TestDeriveCredentialsAndReadBalance(t *testing.T) {
	privateKey := os.Getenv("CLOB_TEST_PRIVATE_KEY")
	if privateKey == "" {
		t.Fatal("CLOB_TEST_PRIVATE_KEY is required for authenticated integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	bootstrap, err := clobclient.New(clobclient.Config{
		PrivateKey: privateKey,
		HTTPClient: integrationClientHTTP(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := bootstrap.DeriveCredentials(ctx)
	if err != nil {
		t.Fatalf("derive credentials: %v", err)
	}
	client, err := clobclient.New(clobclient.Config{
		PrivateKey:  privateKey,
		Credentials: credentials,
		HTTPClient:  integrationClientHTTP(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.BalanceAllowance(ctx, "COLLATERAL", ""); err != nil {
		t.Fatalf("read balance/allowance: %v", err)
	}
	if _, _, err := client.OpenOrders(ctx, ""); err != nil {
		t.Fatalf("read open orders: %v", err)
	}
	if _, err := client.Notifications(ctx); err != nil {
		t.Fatalf("read notifications: %v", err)
	}
	markets, _, err := client.SamplingMarkets(ctx, 1)
	if err != nil || len(markets) == 0 || len(markets[0].Tokens) == 0 {
		t.Fatalf("sampling market for signing: %v", err)
	}
	if _, err := client.CreateOrder(ctx, clobclient.UserOrder{TokenID: markets[0].Tokens[0].TokenID, Side: clobclient.SideBuy, Price: 0.50, Shares: 5, OrderType: clobclient.OrderTypeFOK}); err != nil {
		t.Fatalf("create signed V2 order: %v", err)
	}
}
