package main

import (
	"fmt"
	"log"
	"os"

	clob "github.com/Cyvadra/polymarket-clob-client"
)

func main() {
	// Configuration from environment variables
	host := getEnv("CLOB_HOST", "https://clob.polymarket.com")
	privateKey := getEnv("PRIVATE_KEY", "")
	funderAddress := getEnv("FUNDER_ADDRESS", "")

	if privateKey == "" {
		log.Fatal("PRIVATE_KEY environment variable is required")
	}

	chainID := 137

	// Create client
	var funder *string
	if funderAddress != "" {
		funder = &funderAddress
	}

	client := clob.NewClobClient(
		host,
		chainID,
		privateKey,
		nil,
		clob.SignatureTypePOLYPROXY,
		funder,
	)

	fmt.Println("Authenticating...")

	// Get API credentials
	nonce := "123456"
	creds, err := client.CreateOrDeriveAPIKey(nonce)
	if err != nil {
		log.Fatalf("Failed to get API key: %v", err)
	}

	client.Creds = creds
	fmt.Println("Authenticated successfully!")

	// Get open orders
	fmt.Println("\n1. Fetching open orders...")
	orders, err := client.GetOpenOrders(&clob.OpenOrderParams{})
	if err != nil {
		log.Printf("Failed to get open orders: %v", err)
	} else {
		fmt.Printf("Found %d open orders\n", len(orders))
		for i, order := range orders {
			fmt.Printf("  Order %d: ID=%s, Side=%s, Price=%s, Size=%s\n",
				i+1, order.ID, order.Side, order.Price, order.OriginalSize)
		}
	}

	// Get trades
	fmt.Println("\n2. Fetching trades...")
	trades, err := client.GetTrades(&clob.TradeParams{})
	if err != nil {
		log.Printf("Failed to get trades: %v", err)
	} else {
		fmt.Printf("Found %d trades\n", len(trades))
		for i, trade := range trades {
			if i < 5 { // Show first 5 trades
				fmt.Printf("  Trade %d: ID=%s, Side=%s, Price=%s, Size=%s\n",
					i+1, trade.ID, trade.Side, trade.Price, trade.Size)
			}
		}
	}

	// Get balance and allowance
	fmt.Println("\n3. Checking balance and allowance...")
	balanceParams := &clob.BalanceAllowanceParams{
		AssetType: clob.AssetTypeCollateral,
	}
	balance, err := client.GetBalanceAllowance(balanceParams)
	if err != nil {
		log.Printf("Failed to get balance: %v", err)
	} else {
		fmt.Printf("Balance: %s\n", balance.Balance)
		fmt.Printf("Allowance: %s\n", balance.Allowance)
	}

	// Example: Cancel a specific order (commented out for safety)
	/*
		orderID := "your-order-id"
		fmt.Printf("\n4. Canceling order %s...\n", orderID)
		response, err := client.CancelOrder(orderID)
		if err != nil {
			log.Printf("Failed to cancel order: %v", err)
		} else {
			fmt.Printf("Order canceled: %v\n", response.Success)
		}
	*/

	// Example: Cancel all orders (commented out for safety)
	/*
		fmt.Println("\n5. Canceling all orders...")
		err = client.CancelAll()
		if err != nil {
			log.Printf("Failed to cancel all orders: %v", err)
		} else {
			fmt.Println("All orders canceled successfully!")
		}
	*/

	fmt.Println("\nOrder management operations complete!")
}

func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}
