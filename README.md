# Polymarket CLOB Go Client

An opinionated, context-aware Go client for Polymarket's CLOB API.

## Capabilities

- L1 credential derivation and L2 HMAC authentication
- V2 EIP-712 orders for EOA, Gnosis Safe, and Poly1271 signatures
- Tick size, fee rate, and neg-risk resolution before signing
- Public market, book, pricing, history, and batch-book queries
- Authenticated orders, balances, trades, notifications, scoring, and cancellation
- Reconnecting market/user WebSocket streams
- Optional execution resolution and inventory accounting packages
- Separate Gamma discovery and Data API position clients, or one aggregate
  `polymarket.Client`

## Quick Start

```go
client, err := clobclient.New(clobclient.Config{
    PrivateKey: os.Getenv("POLYMARKET_PRIVATE_KEY"),
})
if err != nil { log.Fatal(err) }

credentials, err := client.DeriveCredentials(ctx)
if err != nil { log.Fatal(err) }

client, err = clobclient.New(clobclient.Config{
    PrivateKey: os.Getenv("POLYMARKET_PRIVATE_KEY"),
    Credentials: credentials,
    QPS: 10,
})
```

All network methods require a `context.Context`. Order submission never
automatically retries, since a timed-out request can still have been accepted.

## Verification

```sh
go test ./...
go test -race ./...
go vet ./...
CLOB_TEST_PROXY=http://127.0.0.1:7890 go test -tags=integration ./integration
```

Authenticated integration tests additionally require `CLOB_TEST_PRIVATE_KEY`.
They derive credentials locally and only perform read-only account requests.
