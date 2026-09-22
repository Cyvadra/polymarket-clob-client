# Polymarket CLOB Go Client

A context-aware Go client for the Polymarket CLOB API, plus `executiond`: a standalone, durable execution service for Polymarket trading systems.

## Features

- L1 credential derivation and L2 HMAC authentication
- V2 EIP-712 orders for EOA, Gnosis Safe, and Poly 1271 signatures
- Tick size, fee rate, and neg-risk resolution before signing
- Public market, order book, pricing, history, and batch order book queries
- Authenticated orders, balances, trades, notifications, scoring, and cancellation
- Authenticated user WebSocket stream with automatic reconnection
- Per-market fee schedules, so fills can be priced with the fee the exchange actually charges
- Durable execution intents, signed-order recovery, `expires_at` cancellation, and order-state reconciliation
- Order and fill handling from the authenticated user stream, with durable position accounting
- Strategy-facing position features: position size, available and reserved shares, entry price, entry time, and open lot count

## Execution runtime

`cmd/executiond` is the execution component of a three-part trading system:

```text
pmm market features -> strategy -> executiond -> Polymarket CLOB
                              ^             |
                              +-- position features and execution results
```

`pmm` produces market features and publishes quote snapshots. The strategy publishes open and close requests. `executiond` signs and submits orders, closes positions, consumes authenticated account events, persists order state, keeps position accounting, and uses the latest quote snapshot to plan the initial child order for the advanced execution styles.

The service keeps durable state in PostgreSQL and exchanges strategy messages over core NATS. Request delivery is deliberately at-most-once: an expired or lost request is never replayed. An open request that decodes but fails validation receives a failure result. A close request gets no ACK or SUCCESS; its terminal result is reported only when the order actually ends. Invalid JSON cannot be answered. Run exactly one `executiond` instance per wallet.

### NATS contract

The versioned field rules, examples, delivery semantics, and compatibility rules are in [docs/protocol/nats-v1.md](docs/protocol/nats-v1.md). Strategy integrations should follow that document rather than import the runtime Go packages.

| Subject | Direction | Payload | Purpose |
| --- | --- | --- | --- |
| `strategy.execution.open` | strategy -> executiond | `ExecutionOpenRequest` | Request one open order. |
| `execution.open.result` | executiond -> strategy | `ExecutionOpenResult` | Terminal success or failure of an open request. |
| `strategy.execution.close` | strategy -> executiond | `ExecutionCloseRequest` | Request a position close. |
| `execution.close.result` | executiond -> strategy | `ExecutionCloseResult` | Terminal success or failure of a close request. |
| `pmm.market.quotes` | market data -> executiond | `marketquotes.Snapshot` | Latest quote snapshot for the advanced execution styles. |
| `execution.order.event` | executiond -> observers | `ExecutionOrderEvent` | Durable order lifecycle transitions. |
| `position.features.<condition_id>.<token_id>` | executiond -> strategy | `PositionFeature` | Latest durable position snapshot. |
| `strategy.execution.position.query` | strategy -> executiond (request/reply) | `PositionQueryRequest` | Query current positions; the reply goes to the request's reply subject. |

### Opening a position

An open request requires `unique_tag`, `strategy`, `condition_id`, `token_id`, `outcome`, `side`, `limit_price`, and `time_in_force`, plus a future `expires_at` or a positive `policy.complete_within_ms`. `target_usd` is the only sizing field; share counts are derived from the planned price.

`unique_tag` is the strategy lane key. It isolates parallel open and close signals on the same asset, and it is not an idempotency key.

The caller must set `policy.style` explicitly:

| Style | Behavior |
| --- | --- |
| `LIMIT` | Creates the initial child order directly from the request's limit price. |
| `MAKER_POST_ONLY` | Plans the child's price, post-only flag, and time in force from the latest quote snapshot. |
| `TAKER_AGGRESSIVE` | Prices the child aggressively from the signal price, capped by the request's limit, and turns a `GTC` time in force into `FAK`. |

Snapshots older than `policy.quote_max_age_ms` are ignored. Before signing, prices are aligned to the market tick in the passive direction (buys down, sells up), and share counts are floored to the precision the exchange encodes.

If `EXECUTION_MAX_OPEN_BUY_NOTIONAL_USD` is set, a buy that would push the total notional of open BUY reservations over the cap is rejected with `EXPOSURE_LIMIT`.

An open result is published only when the open reaches a terminal state: any fill, including a partial one, is a success, and a cancellation with no fill is a failure. Accepting a resting order is never reported as success. Whenever a result carries `filled_shares > 0` it also carries `average_price`.

### Closing a position

A close request targets a lane by `condition_id + asset_id + unique_tag`.

| Mode | Behavior |
| --- | --- |
| `LIMIT_CLOSE` | Places a normal limit sell for the lane's position. It cancels any previous close on the lane but leaves a resting open buy working, so an entry that is still filling keeps filling. |
| `FORCE_CLOSE` | Cancels every order on the lane, including the open buy, then submits an internal `SELL 0.01 FAK`. It never emits a close result; confirm the exit from `position.features.*`. |

Further behavior worth knowing:

- **Replacement.** A newer close on the same lane cancels the older one and suppresses its terminal result. The new close is retried at most every 200ms, bounded by `policy.cancel_replace_timeout_ms`.
- **Close maintenance.** A `LIMIT_CLOSE` is sized for the whole lane. Once the position stops growing, a maintenance loop cancels the resting close and re-places it for the current size. If the position is below the market's minimum order size (`BELOW_MIN_ORDER_SIZE`), `executiond` keeps the shares and retries as the lane grows, and takes the residual with a FAK if the bid reaches the close's limit price before settlement.
- **Unsettled buys.** A sell placed moments after the buy can be rejected with `not enough balance / allowance` while the bought tokens settle on chain. Both close modes retry that rejection about once a second for up to 30 seconds.
- **Terminal results.** A `LIMIT_CLOSE` emits exactly one `execution.close.result`, when its sell child resolves or when placement is refused. A malformed request is dropped, and a close on a lane with no shares is treated as already complete.

Closes are dispatched per lane, so retries on one lane never delay another.

### Position queries

`strategy.execution.position.query` is a request/reply query. With no filter it returns every current position; it can also filter by `condition_id`, `market_id`, or `unique_tag`. The reply uses the same shape as `PositionFeature`.

### Durability and recovery

An accepted order is persisted before it is sent to the exchange. If the process stops after persisting, `executiond` recovers `SIGNED` orders at startup. A submission with an unknown outcome is reconciled against CLOB REST order status.

Two independent loops run once per second:

- **Deadline enforcement** cancels orders that have passed their `expires_at`.
- **Close maintenance** keeps resting closes sized to their positions.

They are separate so that slow maintenance can never delay a cancellation. Each loop times its own passes and reports one that takes ten intervals or more, once when it starts overrunning and once when it recovers, through the daemon's error log.

The reconciliation loop also backfills account trades from CLOB REST and feeds them through the same idempotent fill path as the user stream, repairing any fills missed while the WebSocket was down. An unknown submission whose order cannot be found first enters `UNKNOWN_RECONCILE`; it is failed and its reservation released only if it still returns 404 after the missing-order grace period.

### Latency and fees

Signing the first order on a token needs its tick size, minimum order size, and neg-risk flag, and pricing its fills needs the market's fee schedule. When a market first appears in `pmm.market.quotes`, `executiond` warms all of this in the background (at most four markets at a time), so the first order does not wait on a chain of REST calls.

The exchange can report a fee rate of 0 on trades it actually charged. `executiond` therefore prices each fill from the market fee schedule (`rate * shares * (p * (1 - p))^exponent`, floored to five decimals, and nothing for a maker when the market is taker-only). Fees are informational: a failed fee lookup is reported and the fill is stored without a fee rather than delaying the position.

### Position accounting

The authenticated CLOB user stream is the only account-event input to `executiond`. Fill IDs make repeated deliveries idempotent, and the fill lifecycle (`MATCHED`, `MINED`, `CONFIRMED`, `FAILED`) is persisted. Positions are computed from the raw share counts the exchange reports, with no estimated fee deducted. If a fill later becomes `FAILED`, its effect on the position is reversed by exactly the same share count.

`PositionFeature.open_lots` counts how many separate open requests make up the current position, one lot per intent however many child orders or partial fills it took. It lets a strategy that caps how many times a lane may buy recover that count after a restart. It is meaningful only when `has_position` is true, and an absent value on an open position means "unknown", not zero.

The published position is an execution view, not a settlement or redemption engine. On-chain settlement, redemption, and cross-system fund reconciliation are outside the scope of `executiond`.

### Package layout

| Path | Purpose |
| --- | --- |
| repository root | The CLOB API client (`clobclient`) used by `executiond`. |
| `cmd/executiond` | Composition root of the execution service. |
| `cmd/executiontest` | Real-money NATS black-box lifecycle test. |
| `internal/execution/protocol` | Go wire types private to `executiond`. The external protocol is [docs/protocol/nats-v1.md](docs/protocol/nats-v1.md). |
| `pkg/accountfeed`, `pkg/executor`, `pkg/reconciler`, `pkg/store` | Service implementation packages, kept modular and testable. |
| `pkg/marketquotes`, `pkg/positionfeatures`, `pkg/natsbus`, `pkg/statemachine` | Quote cache, position publisher, NATS bus, and order state machine. |
| `examples/` | Runnable client examples for public market data and an authenticated client. |
| `scripts/` | pm2 deployment; see [scripts/README.md](scripts/README.md). |

### Running `executiond`

`executiond` needs only a signing key. The preferred form is the encrypted keystore pair `POLYMARKET_PRIVATE_KEY_FILE` / `POLYMARKET_PRIVATE_KEY_PASSPHRASE_FILE`, created with `go run ./cmd/polykey encrypt`; `POLYMARKET_PRIVATE_KEY` holds the raw hex key instead and is meant for local development. Set one form or the other, never both. It derives the L2 API credentials (key, secret, passphrase) from the private key at startup, using `GET /auth/derive-api-key` and calling `POST /auth/api-key` only when the signer has no credentials yet. Derivation is idempotent, so restarts never create duplicates, and the credentials should not be configured by hand.

| Variable | Required | Default |
| --- | --- | --- |
| `POLYMARKET_PRIVATE_KEY_FILE` | Yes, unless `POLYMARKET_PRIVATE_KEY` is set | none |
| `POLYMARKET_PRIVATE_KEY_PASSPHRASE_FILE` | Yes, with `POLYMARKET_PRIVATE_KEY_FILE` | none |
| `POLYMARKET_PRIVATE_KEY` | Yes, unless the keystore pair above is set | none |
| `EXECUTION_NATS_URL` | No | `nats://127.0.0.1:4222` |
| `EXECUTION_POSTGRES_URL` | No | `postgres://user:password@127.0.0.1:5432/execution?sslmode=disable` (a placeholder; replace it outside local development) |
| `EXECUTION_MAX_OPEN_BUY_NOTIONAL_USD` | No | no cap |
| `POLYMARKET_PROXY_URL` | No | direct connection |

`executiond` builds its CLOB client straight from the environment and does not discover proxy wallets. It signs as an EOA (`signature_type=0`) by default. **If the trading funds are in a Polymarket proxy wallet (Safe or deposit wallet), you must set both `POLYMARKET_MAKER_ADDRESS` (the proxy wallet address) and `POLYMARKET_SIGNATURE_TYPE` (`1` Poly proxy, `2` Gnosis Safe, `3` Poly 1271).** Setting only one of them produces orders the exchange rejects.

The tuning variables (snapshot and reconciliation intervals, result price wait, and the various timeouts) all have production-usable defaults and do not need to be set at deploy time. They, the full variable list, and guidance on keeping secrets safe are in [docs/configuration.md](docs/configuration.md). [.env.example](.env.example) is a secret-free reference for the variable names.

```sh
go run ./cmd/executiond
```

For a pm2-based deployment to a remote host, see [scripts/README.md](scripts/README.md).

### Real-money NATS black-box test

`cmd/executiontest` connects to an already running, dedicated `executiond` and drives one real-money trade through its whole life over NATS: baseline check, open, fill reconciliation, position accounting, an optional hold, close, and a final flat check. It leaves a Markdown evidence report. It has five required flags: `--condition-id`, `--asset-id`, `--outcome`, `--target-usd`, and `--buy-limit`. Optional flags such as `--sell-limit`, `--close-mode`, `--hold`, and `--negative` add a limit-close scenario, a dwell, and an invalid-schema probe. The NATS address comes from `EXECUTION_NATS_URL`, not from a flag.

**The tool has no confirmation switch: a valid invocation places a real order immediately.** Local validation catches malformed values but not a well-formed wrong asset ID or amount. For a hard exposure ceiling, set `EXECUTION_MAX_OPEN_BUY_NOTIONAL_USD` on the daemon. Prerequisites, run examples, funds risk, and what the test can and cannot prove are in [docs/execution-integration-test.md](docs/execution-integration-test.md).

## Client quick start

```go
client, err := clobclient.New(clobclient.Config{
    PrivateKey: os.Getenv("POLYMARKET_PRIVATE_KEY"),
})
if err != nil { log.Fatal(err) }

credentials, err := client.DeriveCredentials(ctx)
if err != nil { log.Fatal(err) }

client, err = clobclient.New(clobclient.Config{
    PrivateKey:  os.Getenv("POLYMARKET_PRIVATE_KEY"),
    Credentials: credentials,
    QPS:         10,
})
```

Every network method takes a `context.Context`. Order submission is never retried automatically, because a timed-out request may still have been accepted. The execution runtime handles that unknown outcome through its durable recovery path instead of blindly resending the SDK request.

More complete programs are in [examples/public_market_data](examples/public_market_data/main.go) and [examples/authenticated_client](examples/authenticated_client/main.go).

## Verification

```sh
go test ./...
go test -race ./...
go vet ./...
CLOB_TEST_PROXY=http://127.0.0.1:7890 go test -tags=integration ./integration
EXECUTION_TEST_POSTGRES_URL=postgres://... go test -tags=postgres ./pkg/store/postgres
```

The authenticated integration tests additionally need `CLOB_TEST_PRIVATE_KEY`. They derive credentials locally and only make read-only account requests. `CLOB_TEST_SUBMIT_REJECTION=true` opts in to one real unfunded order POST that the exchange is expected to reject.

The `postgres`-tagged store tests run against a live PostgreSQL database and are skipped unless `EXECUTION_TEST_POSTGRES_URL` is set. Each test works in its own lane and removes the rows it wrote, so they can run repeatedly and alongside other data. Point them at a dedicated database anyway.

`./verify.sh` runs the build, the unit tests, the examples build, and `go vet` in one step.
