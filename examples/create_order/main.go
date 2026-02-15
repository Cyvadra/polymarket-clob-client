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

	// Chain ID: 137 for Polygon, 80002 for Amoy testnet
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
		nil, // No credentials yet
		clob.SignatureTypePOLYPROXY,
		funder,
	)

	fmt.Println("Creating/deriving API key...")

	// Create or derive API key
	nonce := "123456"
	creds, err := client.CreateOrDeriveAPIKey(nonce)
	if err != nil {
		log.Fatalf("Failed to get API key: %v", err)
	}

	fmt.Printf("API Key: %s\n", creds.Key)

	// Update client with credentials
	client.Creds = creds

	// Example order parameters
	tokenID := "your-token-id" // Replace with actual token ID
	order := &clob.UserOrder{
		TokenID: tokenID,
		Price:   0.52,
		Size:    10.0,
		Side:    clob.SideBuy,
	}

	options := &clob.CreateOrderOptions{
		TickSize: clob.TickSize0001,
		NegRisk:  boolPtr(false),
	}

	fmt.Println("\nCreating order...")

	// Create the signed order
	signedOrder, err := client.CreateOrder(order, options)
	if err != nil {
		log.Fatalf("Failed to create order: %v", err)
	}

	fmt.Printf("Order created successfully!\n")
	fmt.Printf("Token ID: %s\n", signedOrder.TokenID)
	fmt.Printf("Price: %.2f\n", order.Price)
	fmt.Printf("Size: %.2f\n", order.Size)
	fmt.Printf("Side: %s\n", signedOrder.Side)
	fmt.Printf("Maker Amount: %s\n", signedOrder.MakerAmount)
	fmt.Printf("Taker Amount: %s\n", signedOrder.TakerAmount)
	fmt.Printf("Signature: %s\n", signedOrder.Signature[:20]+"...")

	// Post the order (commented out for safety)
	/*
		fmt.Println("\nPosting order...")
		response, err := client.PostOrder(&clob.PostOrderArgs{
			Order:     *signedOrder,
			OrderType: clob.OrderTypeGTC,
		})
		if err != nil {
			log.Fatalf("Failed to post order: %v", err)
		}

		fmt.Printf("Order posted successfully! Order ID: %s\n", response.OrderID)
	*/
}

func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func boolPtr(b bool) *bool {
	return &b
}
