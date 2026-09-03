# Polymarket CLOB Go Client

A context-aware Go client for Polymarket's CLOB API and a separately runnable,
durable execution service for a Polymarket trading system.

## Capabilities

- L1 credential derivation and L2 HMAC authentication
- V2 EIP-712 orders for EOA, Gnosis Safe, and Poly1271 signatures
- Tick size, fee rate, and neg-risk resolution before signing
- Public market, book, pricing, history, and batch-book queries
- Authenticated orders, balances, trades, notifications, scoring, and cancellation
- Reconnecting market/user WebSocket streams
- Durable execution intents, signed-order recovery, deadline cancellation, and
  order-state reconciliation
- Authenticated user-stream order and fill processing with durable position
  accounting
- Strategy-facing position features: position, available and reserved shares,
  entry price, and entry time
- Separate Gamma discovery and Data API position clients, or one aggregate
  `polymarket.Client`

## Execution Runtime

`cmd/executiond` is the execution component in a three-part trading program:

```text
pmm market features -> strategy -> executiond -> Polymarket CLOB
                              ^             |
                              +-- position features and intent acknowledgements
```

`pmm` owns market-feature production; `executiond` does not consume pmm
features. The strategy resolves market identifiers and publishes a complete
execution intent. `executiond` owns signing, submission, cancellation,
authenticated account events, durable order state, and position accounting.

The service uses PostgreSQL for durable state and core NATS for strategy
messages. Intent delivery is intentionally at-most-once: an expired or lost
intent is not replayed. Every received intent is acknowledged on the execution
acknowledgement subject, so a strategy can safely treat a missing acknowledgement
as not opened.

### NATS Contracts

All payloads use `schema_version: "execution.v1"`.

| Subject | Direction | Payload | Purpose |
| --- | --- | --- | --- |
| `strategy.execution.intent` | strategy -> executiond | `ExecutionIntent` | Request an `OPEN` or `CLOSE` order. |
| `execution.intent.ack` | executiond -> strategy | `ExecutionIntentAck` | Acceptance, rejection, terminal completion, partial fill, expiry, or failure. |
| `execution.order.event` | executiond -> observers | `ExecutionOrderEvent` | Durable order lifecycle transition. |
| `position.features.<condition_id>.<token_id>` | executiond -> strategy | `PositionFeature` | Latest durable position snapshot. |

`ExecutionIntent` requires an intent ID, idempotency key, strategy, kind,
condition ID, token ID, outcome, side, shares, limit price, time-in-force, and
an expiry or completion deadline. A `CLOSE` intent must be a sell. A sell is
reserved atomically against available inventory; a close with no sellable
position is rejected with `NO_POSITION`.

An accepted order is persisted before its external submission. If a process
stops after persistence, `executiond` resumes `SIGNED` orders on startup. A
submission result that is not known is reconciled from CLOB REST state. Orders
past their configured deadline are cancelled.

### Position Accounting

The authenticated CLOB user stream is the only account-event ingress in
`executiond`. Fill IDs make repeated delivery idempotent. Trade lifecycle
states (`MATCHED`, `MINED`, `CONFIRMED`, `FAILED`) are persisted. A taker BUY
credits net outcome shares using the configured Polymarket fee formula; if that
trade later becomes `FAILED`, its position effect is reversed using the same
credited-share amount.

The published position state is an execution view, not a settlement or
redemption engine. See [TODO.md](TODO.md) for the remaining reconciliation and
settlement work.

### Running `executiond`

`executiond` requires the normal authenticated CLOB environment variables
(`POLYMARKET_PRIVATE_KEY`, `POLYMARKET_API_KEY`,
`POLYMARKET_API_SECRET`, and `POLYMARKET_API_PASSPHRASE`) plus:

| Variable | Required | Default |
| --- | --- | --- |
| `EXECUTION_NATS_URL` | yes | - |
| `EXECUTION_POSTGRES_URL` | yes | - |
| `EXECUTION_POSITION_FEATURE_INTERVAL` | no | `500ms` |
| `EXECUTION_RECONCILE_INTERVAL` | no | `30s` |
| `EXECUTION_CONNECT_TIMEOUT` | no | `10s` |
| `EXECUTION_SHUTDOWN_GRACE_PERIOD` | no | `10s` |

```sh
go run ./cmd/executiond
```

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
The execution runtime handles that unknown result through its durable recovery
path rather than by blindly resubmitting the SDK request.

## Verification

```sh
go test ./...
go test -race ./...
go vet ./...
CLOB_TEST_PROXY=http://127.0.0.1:7890 go test -tags=integration ./integration
```

Authenticated integration tests additionally require `CLOB_TEST_PRIVATE_KEY`.
They derive credentials locally and only perform read-only account requests.
