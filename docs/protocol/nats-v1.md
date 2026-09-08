# executiond NATS Protocol v1

`executiond` uses core NATS. Messages are JSON and use `schema_version: "execution.v1"` unless noted otherwise.

## Delivery

- Strategy commands are at-most-once.
- `executiond` publishes order events and results best-effort.
- One `executiond` instance should manage one wallet.

## Subjects

| Subject | Direction | Payload | Meaning |
| --- | --- | --- | --- |
| `strategy.execution.open` | strategy -> executiond | `ExecutionOpenRequest` | Submit one open order request. |
| `execution.open.result` | executiond -> strategy | `ExecutionOpenResult` | Success or failure for an open request. |
| `strategy.execution.close` | strategy -> executiond | `ExecutionCloseRequest` | Close current position using limit-close or force-close mode. |
| `execution.close.result` | executiond -> strategy | `ExecutionCloseResult` | Success or failure for a close request. |
| `execution.order.event` | executiond -> observers | `ExecutionOrderEvent` | Durable order-state transition. |
| `pmm.market.quotes` | market data -> executiond | `marketquotes.Snapshot` | Cached quote snapshot for tactics. |
| `position.features.<condition_id>.<token_id>` | executiond -> strategy | `PositionFeature` | Latest position snapshot. |
| `strategy.execution.position.query` | strategy -> executiond (request/reply) | `PositionQueryRequest` | Query positions by condition or market. |

Subject tokens must not be empty or include `*` or `>`.

`execution.order.event` is an observer subject for durable order-state transitions (including internal force-close children and close orders). Its `intent_id` is a server-side execution id for correlation/debugging, not a client-supplied key. The strategy normally relies on the `*.result` subjects and `position.features.*`; it may ignore order events.

## ExecutionOpenRequest

Required fields are `schema_version`, `strategy`, `condition_id`, `token_id`, `outcome`, `side`, `limit_price`, and `time_in_force`. `target_usd` sizes the open request. A future `expires_at` or positive `policy.complete_within_ms` is required. Signals are delivered at most once and never replayed, so no client-supplied idempotency key is required; executiond assigns each execution a server-side identity.

| Field | Rules |
| --- | --- |
| `side` | `BUY` or `SELL`. |
| `policy.style` | `LIMIT`, `MAKER_POST_ONLY`, or `TAKER_AGGRESSIVE`. |
| `policy.quote_max_age_ms` | Optional freshness bound for quotes. |
| `policy.reprice_interval_ms` | Reserved; non-zero rejected. |
| `policy.max_reprices` | Reserved; non-zero rejected. |
| `policy.soft_close_after_ms` | Reserved; non-zero rejected. |
| `policy.force_close_after_ms` | Reserved; non-zero rejected. |
| `policy.cancel_replace_timeout_ms` | Reserved; non-zero rejected. |

Prices are aligned to tick size before signing. Share sizes are floored to exchange precision.

## ExecutionCloseRequest

Close requests operate on `condition_id + asset_id` rather than an intent ID.

| Field | Rules |
| --- | --- |
| `schema_version` | Required, `execution.v1`. |
| `mode` | `LIMIT_CLOSE` or `FORCE_CLOSE`. |
| `limit_price` | Required for `LIMIT_CLOSE`. |
| `asset_id` | The token/asset being closed. |

`LIMIT_CLOSE` submits a normal sell close. `FORCE_CLOSE` submits a `SELL 0.01 FAK` exit for the remaining position.

## Results

Open and close results identify the affected position (`condition_id` + `token_id` / `asset_id` + `side`) so the strategy can attribute them without a correlation key. They use `status: "SUCCEEDED"` or `"FAILED"` plus optional `reason_code`, `reason`, `filled_shares`, and `average_price` fields.

Open results are emitted **only at terminal resolution** of an open — when the child order reaches a fill (any amount counts as success, including a partial fill) or is cancelled without any fill (failure). executiond never publishes an open `SUCCEEDED` merely because a resting order was accepted, so a success always means shares were actually bought. `filled_shares` is populated on terminal open results.

Close results report *dispatch*: `FORCE_CLOSE` reports `SUCCEEDED` once the 0.01 FAK exit is submitted (best effort — success even if the sell never fills), and `LIMIT_CLOSE` reports success once the limit sell is submitted.

`average_price` is currently reserved and not populated on either result; the authoritative entry/exit price is conveyed on `position.features.*`.

## Reason codes

`INVALID_INTENT`, `UNSUPPORTED_EXECUTION_STYLE`, `UNIMPLEMENTED_POLICY`, `NO_POSITION`, `ACTIVE_SELL_RESERVATION`, `EXPOSURE_LIMIT`, `UNPLANNABLE`, `ORDER_REJECTED`, `EXECUTION_FAILED`.

## Examples

```json
{
  "schema_version": "execution.v1",
  "strategy": "late-gap",
  "condition_id": "0xcondition",
  "token_id": "12345",
  "outcome": "Up",
  "side": "BUY",
  "target_usd": "12.5",
  "limit_price": "0.42",
  "time_in_force": "GTC",
  "expires_at": "2026-09-04T12:05:00Z",
  "policy": { "style": "LIMIT", "complete_within_ms": 30000 }
}
```

```json
{
  "schema_version": "execution.v1",
  "strategy": "late-gap",
  "condition_id": "0xcondition",
  "asset_id": "12345",
  "outcome": "Up",
  "mode": "FORCE_CLOSE"
}
```
