package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

func main() {
	privateKey := os.Getenv("POLYMARKET_PRIVATE_KEY")
	if privateKey == "" {
		log.Fatal("POLYMARKET_PRIVATE_KEY is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bootstrap, err := clobclient.New(clobclient.Config{PrivateKey: privateKey})
	if err != nil {
		log.Fatal(err)
	}
	credentials, err := bootstrap.DeriveCredentials(ctx)
	if err != nil {
		log.Fatal(err)
	}
	client, err := clobclient.New(clobclient.Config{PrivateKey: privateKey, Credentials: credentials})
	if err != nil {
		log.Fatal(err)
	}
	balance, err := client.BalanceAllowance(ctx, "COLLATERAL", "")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("collateral balance=%s allowance=%s\n", balance.Balance, balance.Allowance)
}
