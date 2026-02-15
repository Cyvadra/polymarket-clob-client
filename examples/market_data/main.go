package main

import (
	"fmt"
	"log"

	clob "github.com/Cyvadra/polymarket-clob-client"
)

func main() {
	// Configuration
	host := "https://clob.polymarket.com"
	chainID := 137

	// Create a basic client (no auth needed for public endpoints)
	client := clob.NewClobClient(
		host,
		chainID,
		"", // No private key needed for public data
		nil,
		clob.SignatureTypeEOA,
		nil,
	)

	// Example token ID (replace with actual token ID)
	tokenID := "21742633143463906290569050155826241533067272736897614950488156847949938836455"

	fmt.Println("Fetching market data...")

	// Get order book
	fmt.Println("\n1. Getting order book...")
	book, err := client.GetOrderBook(tokenID)
	if err != nil {
		log.Printf("Failed to get order book: %v", err)
	} else {
		fmt.Printf("Market: %s\n", book.Market)
		fmt.Printf("Asset ID: %s\n", book.AssetID)
		fmt.Printf("Tick Size: %s\n", book.TickSize)
		fmt.Printf("Neg Risk: %v\n", book.NegRisk)
		fmt.Printf("Number of Bids: %d\n", len(book.Bids))
		fmt.Printf("Number of Asks: %d\n", len(book.Asks))
		if len(book.Bids) > 0 {
			fmt.Printf("Best Bid: %s @ %s\n", book.Bids[0].Size, book.Bids[0].Price)
		}
		if len(book.Asks) > 0 {
			fmt.Printf("Best Ask: %s @ %s\n", book.Asks[0].Size, book.Asks[0].Price)
		}
	}

	// Get midpoint price
	fmt.Println("\n2. Getting midpoint price...")
	mid, err := client.GetMidpoint(tokenID)
	if err != nil {
		log.Printf("Failed to get midpoint: %v", err)
	} else {
		fmt.Printf("Midpoint: %.4f\n", mid)
	}

	// Get buy price
	fmt.Println("\n3. Getting buy price...")
	side := clob.SideBuy
	buyPrice, err := client.GetPrice(tokenID, &side)
	if err != nil {
		log.Printf("Failed to get buy price: %v", err)
	} else {
		fmt.Printf("Buy Price: %.4f\n", buyPrice)
	}

	// Get sell price
	fmt.Println("\n4. Getting sell price...")
	sellSide := clob.SideSell
	sellPrice, err := client.GetPrice(tokenID, &sellSide)
	if err != nil {
		log.Printf("Failed to get sell price: %v", err)
	} else {
		fmt.Printf("Sell Price: %.4f\n", sellPrice)
	}

	// Get server time
	fmt.Println("\n5. Getting server time...")
	serverTime, err := client.GetServerTime()
	if err != nil {
		log.Printf("Failed to get server time: %v", err)
	} else {
		fmt.Printf("Server Time: %d\n", serverTime)
	}

	fmt.Println("\nMarket data fetch complete!")
}
