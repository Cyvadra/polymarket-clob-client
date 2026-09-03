package main

import (
	"context"
	"fmt"
	"log"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

func main() {
	client, err := clobclient.New(clobclient.Config{QPS: 10})
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	markets, _, err := client.SamplingMarkets(ctx, 5)
	if err != nil {
		log.Fatal(err)
	}
	for _, market := range markets {
		fmt.Printf("%s: %s\n", market.ConditionID, market.Question)
	}
}
