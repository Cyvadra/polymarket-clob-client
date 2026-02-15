# Polymarket CLOB Client - Go Implementation

A Go client for the Polymarket Central Limit Order Book (CLOB) API. This is a port of the official TypeScript [@polymarket/clob-client](https://github.com/Polymarket/clob-client).

## Features

- ✅ Full API coverage for Polymarket CLOB
- ✅ EIP712 signature support for order signing
- ✅ L1 (wallet) and L2 (API key) authentication
- ✅ Order creation, posting, and cancellation
- ✅ Market data queries (order books, prices, trades)
- ✅ Automatic retry logic with exponential backoff
- ✅ Type-safe API with Go structs

## Installation

```bash
go get github.com/Cyvadra/polymarket-clob-client
```

## Quick Start

```go
package main

import (
    "fmt"
    "log"
    
    clob "github.com/Cyvadra/polymarket-clob-client"
)

func main() {
    // Configuration
    host := "https://clob.polymarket.com"
    chainID := 137 // Polygon mainnet
    privateKey := "0x..." // Your private key
    funder := "0x..." // Your Polymarket profile address (optional)
    
    // Create client without credentials first
    client := clob.NewClobClient(
        host,
        chainID,
        privateKey,
        nil, // No credentials yet
        clob.SignatureTypePOLYPROXY, // or SignatureTypeEOA
        &funder,
    )
    
    // Create or derive API key
    nonce := "12345"
    creds, err := client.CreateOrDeriveAPIKey(nonce)
    if err != nil {
        log.Fatalf("Failed to get API key: %v", err)
    }
    
    // Update client with credentials
    client.Creds = creds
    
    // Create and post an order
    order := &clob.UserOrder{
        TokenID: "your-token-id",
        Price:   0.52,
        Size:    10.0,
        Side:    clob.SideBuy,
    }
    
    options := &clob.CreateOrderOptions{
        TickSize: clob.TickSize0001,
        NegRisk:  boolPtr(false),
    }
    
    response, err := client.CreateAndPostOrder(
        order,
        options,
        clob.OrderTypeGTC,
    )
    if err != nil {
        log.Fatalf("Failed to post order: %v", err)
    }
    
    fmt.Printf("Order posted! ID: %s\n", response.OrderID)
}

func boolPtr(b bool) *bool {
    return &b
}
```

## Core Components

### ClobClient

The main client for interacting with the Polymarket CLOB API.

```go
client := clob.NewClobClient(
    host,        // API endpoint
    chainID,     // Blockchain network (137 for Polygon, 80002 for Amoy)
    privateKey,  // Your private key
    creds,       // API credentials (can be nil initially)
    signatureType, // Signature type (EOA, POLYPROXY, etc.)
    funderAddress, // Optional funder address
)
```

### Authentication

**L1 Authentication (Wallet-based):**
- Used for creating/deriving API keys
- Uses EIP712 signatures

```go
creds, err := client.CreateAPIKey(nonce)
// or
creds, err := client.DeriveAPIKey(nonce)
// or
creds, err := client.CreateOrDeriveAPIKey(nonce)
```

**L2 Authentication (API Key-based):**
- Used for trading operations
- Uses HMAC-SHA256 signatures
- Automatically handled by the client when credentials are set

### Order Management

**Create and Post Order:**

```go
userOrder := &clob.UserOrder{
    TokenID: "token-id",
    Price:   0.52,
    Size:    10.0,
    Side:    clob.SideBuy,
}

options := &clob.CreateOrderOptions{
    TickSize: clob.TickSize0001,
}

response, err := client.CreateAndPostOrder(
    userOrder,
    options,
    clob.OrderTypeGTC,
)
```

**Cancel Order:**

```go
response, err := client.CancelOrder(orderID)
```

**Cancel All Orders:**

```go
err := client.CancelAll()
```

### Market Data

**Get Order Book:**

```go
book, err := client.GetOrderBook(tokenID)
```

**Get Price:**

```go
price, err := client.GetPrice(tokenID, nil)
// or with side
side := clob.SideBuy
price, err := client.GetPrice(tokenID, &side)
```

**Get Midpoint:**

```go
mid, err := client.GetMidpoint(tokenID)
```

**Get Open Orders:**

```go
params := &clob.OpenOrderParams{
    Market: &marketID,
}
orders, err := client.GetOpenOrders(params)
```

**Get Trades:**

```go
params := &clob.TradeParams{
    Market: &marketID,
}
trades, err := client.GetTrades(params)
```

### Balance & Allowance

```go
params := &clob.BalanceAllowanceParams{
    AssetType: clob.AssetTypeCollateral,
}
balance, err := client.GetBalanceAllowance(params)
```

## Types

### Order Types

- `OrderTypeGTC` - Good Till Cancel
- `OrderTypeFOK` - Fill or Kill
- `OrderTypeGTD` - Good Till Date
- `OrderTypeFAK` - Fill and Kill

### Sides

- `SideBuy` - Buy order
- `SideSell` - Sell order

### Chains

- `ChainPolygon` - Polygon mainnet (137)
- `ChainAmoy` - Amoy testnet (80002)

### Signature Types

- `SignatureTypeEOA` - Externally Owned Account
- `SignatureTypePOLYPROXY` - Polymarket Proxy Wallet
- `SignatureTypePOLYGNOSISSAFE` - Gnosis Safe

### Tick Sizes

- `TickSize01` - 0.1
- `TickSize001` - 0.01
- `TickSize0001` - 0.001
- `TickSize00001` - 0.0001

## Error Handling

The client returns standard Go errors. Always check for errors:

```go
response, err := client.CreateAndPostOrder(order, options, orderType)
if err != nil {
    log.Printf("Error: %v", err)
    return
}
```

## Examples

See the [examples](examples/) directory for more detailed examples:

- [Basic order creation](examples/create_order/main.go)
- [Market data queries](examples/market_data/main.go)
- [Order management](examples/order_management/main.go)

## Architecture

The Go implementation follows the same architecture as the TypeScript client:

- **Client Layer**: Main `ClobClient` handles API interactions
- **Order Builder**: Creates and signs orders using EIP712
- **Headers**: Manages L1/L2 authentication headers
- **Signing**: EIP712 and HMAC-SHA256 signature generation
- **HTTP Client**: Handles requests with retry logic

## Differences from TypeScript Client

While maintaining feature parity, the Go implementation has some differences:

1. **Error Handling**: Uses Go's explicit error returns instead of exceptions
2. **Type System**: Uses Go structs with JSON tags instead of TypeScript interfaces
3. **Async**: Synchronous by default (Go doesn't have async/await)
4. **Naming**: Follows Go conventions (PascalCase for exports)

## Testing

```bash
go test ./...
```

## Contributing

Contributions are welcome! Please feel free to submit issues and pull requests.

## License

MIT License - see [LICENSE](LICENSE) file for details.

## Related Projects

- [Official TypeScript Client](https://github.com/Polymarket/clob-client)
- [Polymarket Documentation](https://docs.polymarket.com/)

## Disclaimer

This is an unofficial implementation. Use at your own risk. Always test thoroughly in a testnet environment before using with real funds.