//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

func TestUnfundedOrderSubmissionIsRejected(t *testing.T) {
	if os.Getenv("CLOB_TEST_SUBMIT_REJECTION") != "true" {
		t.Skip("set CLOB_TEST_SUBMIT_REJECTION=true to exercise a real unfunded order POST")
	}
	privateKey := os.Getenv("CLOB_TEST_PRIVATE_KEY")
	if privateKey == "" {
		t.Fatal("CLOB_TEST_PRIVATE_KEY is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	bootstrap, err := clobclient.New(clobclient.Config{PrivateKey: privateKey, HTTPClient: integrationClientHTTP(t)})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := bootstrap.DeriveCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	client, err := clobclient.New(clobclient.Config{PrivateKey: privateKey, Credentials: credentials, HTTPClient: integrationClientHTTP(t)})
	if err != nil {
		t.Fatal(err)
	}
	markets, _, err := client.SamplingMarkets(ctx, 1)
	if err != nil || len(markets) == 0 || len(markets[0].Tokens) == 0 {
		t.Fatalf("sampling market: %v", err)
	}
	_, err = client.SubmitOrder(ctx, clobclient.UserOrder{TokenID: markets[0].Tokens[0].TokenID, Side: clobclient.SideBuy, Price: .50, Shares: 5, OrderType: clobclient.OrderTypeFOK})
	if err == nil {
		t.Fatal("unfunded order unexpectedly succeeded")
	}
	var apiErr *clobclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected CLOB rejection, got %T: %v", err, err)
	}
	if apiErr.StatusCode < 400 || apiErr.StatusCode >= 500 {
		t.Fatalf("expected client rejection, got HTTP %d: %s", apiErr.StatusCode, apiErr.Body)
	}
}
