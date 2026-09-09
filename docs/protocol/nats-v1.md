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

## PositionFeature

`position.features.<condition_id>.<token_id>` and `PositionQueryResponse.positions` use JSON numbers for `entry_price`, `position_size`, `actual_shares`, `available_size`, `reserved_size`, and `seconds_since_entry`. An empty position has `entry_price: null` and `entry_time: null`; zero-valued numeric fields may be omitted. The storage layer may retain decimal text, but it is converted to rounded JSON floating-point values at this wire boundary.

## ExecutionOpenRequest

Required fields are `schema_version`, `unique_tag`, `strategy`, `condition_id`, `token_id`, `outcome`, `side`, `limit_price`, and `time_in_force`. `target_usd` sizes the open request. A future `expires_at` or positive `policy.complete_within_ms` is required. `unique_tag` is the strategy lane key: it separates concurrent open/close signals on the same asset. executiond assigns the execution identity server-side; `unique_tag` is not an idempotency key.

| Field | Rules |
| --- | --- |
| `side` | `BUY` or `SELL`. |
| `policy.style` | `LIMIT`, `MAKER_POST_ONLY`, or `TAKER_AGGRESSIVE`. |
| `policy.quote_max_age_ms` | Optional freshness bound for quotes. |
| `policy.reprice_interval_ms` | Reserved; non-zero rejected. |
| `policy.max_reprices` | Reserved; non-zero rejected. |
| `policy.soft_close_after_ms` | Reserved; non-zero rejected. |
| `policy.force_close_after_ms` | Reserved; non-zero rejected. |
| `policy.cancel_replace_timeout_ms` | Used by repeated close replacement retries. |

Prices are aligned to tick size before signing. Share sizes are floored to exchange precision.

## ExecutionCloseRequest

Close requests operate on `condition_id + asset_id` rather than an intent ID.
Close requests also require `unique_tag` so the execution daemon can target the correct strategy lane.

| Field | Rules |
| --- | --- |
| `schema_version` | Required, `execution.v1`. |
| `mode` | `LIMIT_CLOSE` or `FORCE_CLOSE`. |
| `limit_price` | Required for `LIMIT_CLOSE`. |
| `asset_id` | The token/asset being closed. |

`LIMIT_CLOSE` submits a normal sell close. `FORCE_CLOSE` submits a `SELL 0.01 FAK` exit for the remaining position. If a new close arrives while one is still pending on the same lane, executiond cancels the old close, suppresses its terminal result, and retries the latest close after at least 200ms; `policy.cancel_replace_timeout_ms` bounds that replacement retry loop.

## Results

Open and close results identify the affected position (`unique_tag` + `condition_id` + `token_id` / `asset_id` + `side`) so the strategy can attribute them without a correlation key. They use `status: "SUCCEEDED"` or `"FAILED"` plus optional `reason_code`, `reason`, `filled_shares`, and `average_price` fields. `filled_shares` and `average_price`, when present, are JSON numbers rather than decimal strings.

Open results are emitted **only at terminal resolution** of an open — when the child order reaches a fill (any amount counts as success, including a partial fill) or is cancelled without any fill (failure). executiond never publishes an open `SUCCEEDED` merely because a resting order was accepted, so a success always means shares were actually bought. `filled_shares` is populated on terminal open results.

Close results are emitted only at terminal resolution of the strategy close child. A fill (including a partial fill) reports `SUCCEEDED` with `filled_shares` populated, while a cancel/expiry with no fill reports `FAILED`. executiond does not publish a close ACK or dispatch success when a request is merely accepted. If a close intent has been superseded by a newer close on the same lane, its terminal result is suppressed.

A `LIMIT_CLOSE` emits exactly one close result, when its strategy sell child resolves. `FORCE_CLOSE` has no strategy close child (its `0.01 FAK` exit is an internal child), so it never emits a close result: treat it as fire-and-forget and confirm the exit from `position.features.*`. The same applies to a close that fails before any child order exists (e.g. a malformed request or a submission rejected by the exchange) — the strategy observes that the position never changed and re-closes.

`average_price` is currently reserved and not populated on either result; the authoritative entry/exit price is conveyed on `position.features.*`.

Order events use the same numeric representation for `matched_shares` when the field is present.

## Reason codes

`INVALID_INTENT`, `UNSUPPORTED_EXECUTION_STYLE`, `UNIMPLEMENTED_POLICY`, `NO_POSITION`, `ACTIVE_SELL_RESERVATION`, `EXPOSURE_LIMIT`, `UNPLANNABLE`, `ORDER_REJECTED`, `EXECUTION_FAILED`.

## Examples

```json
{
  "schema_version": "execution.v1",
  "unique_tag": "late-gap",
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
  "unique_tag": "late-gap",
  "strategy": "late-gap",
  "condition_id": "0xcondition",
  "asset_id": "12345",
  "outcome": "Up",
  "mode": "FORCE_CLOSE"
}
```
