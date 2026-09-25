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
| `strategy.execution.close` | strategy -> executiond | `ExecutionCloseRequest` | Close current position using limit-close or force-close mode, or withdraw a working entry with cancel-open mode. |
| `execution.close.result` | executiond -> strategy | `ExecutionCloseResult` | Success or failure for a close request. |
| `execution.order.event` | executiond -> observers | `ExecutionOrderEvent` | Durable order-state transition. |
| `pmm.market.quotes` | market data -> executiond | `marketquotes.Snapshot` | Cached quote snapshot for tactics. |
| `position.features.<condition_id>.<token_id>` | executiond -> strategy | `PositionFeature` | Latest position snapshot. |
| `strategy.execution.position.query` | strategy -> executiond (request/reply) | `PositionQueryRequest` | Query positions by condition or market. |
| `strategy.execution.balance.query` | strategy -> executiond (request/reply) | `BalanceQueryRequest` | Query the wallet's USDC cash and equity. |

Subject tokens must not be empty or include `*` or `>`.

`execution.order.event` is an observer subject for durable order-state transitions (including internal force-close children and close orders). Its `intent_id` is a server-side execution id for correlation/debugging, not a client-supplied key. `unique_tag` carries the lane the order belongs to so an observer can filter the shared subject to its own orders; it is omitted when the event's intent could not be resolved. The strategy normally relies on the `*.result` subjects and `position.features.*`; it may ignore order events.

## Equity-fraction sizing

An open request may carry `target_equity_fraction` (a decimal string, `"0.03"` = 3%) instead of `target_usd`. Exactly one of the two may be set, and only a BUY may use the fraction. executiond resolves it when the request arrives:

`target_usd = floor(fraction × equity_usd, 6 decimals)`, with `equity_usd` as in the balance query below. From there the open is planned exactly as a dollar-sized one. The resolved `target_usd`, the fraction, and the equity it was taken from are stored with the intent.

Every open sized this way uses the same equity. Placing a buy does not change equity, because the order is still cash until it fills, so each concurrent entry gets its full fraction. What entries can run out of is free cash: `cash_usd` minus the notional of working buys and of entries still being placed. An entry larger than that is rejected with `EXPOSURE_LIMIT` rather than shrunk. A partly filled buy commits only its unfilled notional; the filled part has already left cash. A fraction above `EXECUTION_MAX_EQUITY_FRACTION`, or not above zero, is rejected with `INVALID_INTENT`. If equity cannot be read, the open fails with `EXECUTION_FAILED`.

## Balance query

`BalanceQueryRequest` carries only `schema_version`. The reply is a `BalanceQueryResponse`:

- `cash_usd`: the wallet's USDC collateral balance as the CLOB reports it. Resting orders are not deducted: a working buy is still cash until it fills. The read is cached for `EXECUTION_BALANCE_CACHE_TTL`; `cash_as_of` says when it was taken.
- `positions_value_usd`: the sum over recorded lanes holding shares of `position_size` × the token's best bid in the latest PMM quote. A fresh quote with no bid values the lane at zero. A lane with no quote, or one older than `EXECUTION_EQUITY_MAX_QUOTE_AGE`, is valued at its entry price and counted in `unmarked_positions`.
- `equity_usd`: `cash_usd + positions_value_usd`.
- `positions`: how many lanes were valued.

Only positions executiond recorded are valued; shares the wallet holds outside any lane are not. If the balance cannot be read, the reply carries `error` and zero values, never a guessed equity.

## PositionFeature

`position.features.<condition_id>.<token_id>` and `PositionQueryResponse.positions` use JSON numbers for `entry_price`, `position_size`, `actual_shares`, `available_size`, `reserved_size`, and `seconds_since_entry`. An empty position has `entry_price: null` and `entry_time: null`; zero-valued numeric fields may be omitted. The storage layer may retain decimal text, but it is converted to rounded JSON floating-point values at this wire boundary.

`position_size` and `actual_shares` are the net (BUY − SELL) of the share counts the exchange reported for the lane's fills, with no estimated fee deducted. The fields of an open request (`target_usd`, `limit_price`, and the shares planned from them) describe intent only; they never set a position's size, which follows actual fills alone.

`open_lots` is how many separate open requests make up the position the lane holds now: one lot per intent, however many child orders or partial fills it took, counting only fills since the position was last empty. It is meaningful only when `has_position` is true: an empty position has zero lots, which the wire omits, and an executiond that predates the field omits it too — so a consumer that needs it must treat an absent value on an open position as "unknown" rather than as zero lots. The size cannot stand in for it: several lots pool into one `position_size`, so a strategy that bounds how many times a lane may buy has no other way to recover that count after a restart.

## ExecutionOpenRequest

Required fields are `schema_version`, `unique_tag`, `strategy`, `condition_id`, `token_id`, `outcome`, `side`, `limit_price`, and `time_in_force`. `target_usd` sizes the open request, or `target_equity_fraction` sizes it from wallet equity (see Equity-fraction sizing). A future `expires_at` or positive `policy.complete_within_ms` is required. `unique_tag` is the strategy lane key: it separates concurrent open/close signals on the same asset. executiond assigns the execution identity server-side; `unique_tag` is not an idempotency key.

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
| `mode` | `LIMIT_CLOSE`, `FORCE_CLOSE`, or `CANCEL_OPEN`. |
| `limit_price` | Required for `LIMIT_CLOSE`; ignored by the other two modes. |
| `asset_id` | The token/asset being closed. |

`LIMIT_CLOSE` submits a normal sell close. `FORCE_CLOSE` submits a `SELL 0.01 FAK` exit for the remaining position. If a new close arrives while one is still pending on the same lane, executiond cancels the old close, suppresses its terminal result, and retries the latest close after at least 200ms; `policy.cancel_replace_timeout_ms` bounds that replacement retry loop.

`CANCEL_OPEN` withdraws the lane's working entry and does nothing else: it sells nothing, writes no close intent, and leaves a resting close alone, because that exit is still wanted. It needs no `limit_price`. A lane whose entry has already filled or is already gone has nothing to cancel, and the request is a no-op rather than a failure — the sender emits one per order without knowing whether the exchange still holds it.

## Results

Open and close results identify the affected position (`unique_tag` + `condition_id` + `token_id` / `asset_id` + `side`) so the strategy can attribute them without a correlation key. They use `status: "SUCCEEDED"` or `"FAILED"` plus optional `reason_code`, `reason`, `filled_shares`, and `average_price` fields. `filled_shares` and `average_price`, when present, are JSON numbers rather than decimal strings.

Open results are emitted **only at terminal resolution** of an open — when the child order reaches a fill (any amount counts as success, including a partial fill) or is cancelled without any fill (failure). executiond never publishes an open `SUCCEEDED` merely because a resting order was accepted, so a success always means shares were actually bought. `filled_shares` is populated on terminal open results.

Close results are emitted only at terminal resolution of the strategy close child. A fill (including a partial fill) reports `SUCCEEDED` with `filled_shares` populated, while a cancel/expiry with no fill reports `FAILED`. executiond does not publish a close ACK or dispatch success when a request is merely accepted. If a close intent has been superseded by a newer close on the same lane, its terminal result is suppressed.

A `LIMIT_CLOSE` emits exactly one close result: when its strategy sell child resolves, or `FAILED` with the rejection's `reason_code` when placement is refused (for example the exchange rejects the sell or the close cannot be planned). Two cases emit nothing: a malformed request (wrong schema version, missing fields, invalid limit price or time in force) is dropped, and a close on a lane with no shares left is treated as already complete. `FORCE_CLOSE` has no strategy close child (its `0.01 FAK` exit is an internal child), so it never emits a close result: treat it as fire-and-forget and confirm the exit from `position.features.*`. `CANCEL_OPEN` emits no close result either: the cancelled entry's own open result still resolves terminally and reports how much filled before the cancel landed.

A `LIMIT_CLOSE` is sized for the whole lane and leaves a resting open buy working, so an entry that is still filling keeps growing behind it. executiond tracks that: once the position stops moving it cancels the resting close and re-places it for the current size. The replacement covers the whole position rather than the increment, because an increment on its own is usually below the market's minimum order size and would be refused. A `FORCE_CLOSE` still cancels every order on the lane, including the open buy — it exits before settlement and needs the position to stop moving.

If the exchange refuses a sell over its size, executiond adopts the holding the rejection reports (`balance: N`, in 1e6 units) as the lane's size before retrying, so the retry is planned from the wallet's own view rather than the local fill ledger. A reported balance of zero is treated as the unsettled case below, not as an empty position.

A sell submitted moments after the buy that produced the position can be rejected with `not enough balance / allowance` because the bought tokens have not settled on chain yet. For both close modes, executiond retries that rejection with a fresh child order about once a second for up to 30 seconds (or `policy.cancel_replace_timeout_ms`, if longer) before giving up. Closes are dispatched per lane (`strategy` + `unique_tag` + `condition_id` + `asset_id`): closes on different lanes run concurrently, so one lane's retry does not delay another's, while closes on the same lane still run in order and only the most recent pending close on a lane is kept.

`average_price` is the share-weighted average price of the child order's fills, and it is the authoritative realized entry price for a result: **whenever `filled_shares > 0`, `average_price` is present**. It comes from the submission response for an order that matched on arrival, and otherwise from the order's recorded fills.

The order message that ends an order can arrive ahead of the trade messages that price it, so a result whose fills have not landed yet waits for them, up to `EXECUTION_RESULT_PRICE_WAIT` (3500ms by default), before publishing. If they still have not landed, the result carries the order's own limit price, which bounds the fill on the side it was placed. A result with `filled_shares: 0` has no price and omits the field. `position.features.*` remains the authority for a lane's aggregate entry price across several orders.

A terminal result is published once per intent child. The exchange repeats an order message — the same status and the same `size_matched` several times within milliseconds — and those repeats move nothing, so they emit no further result.

Order events use the same numeric representation for `matched_shares` when the field is present.

## Reason codes

`INVALID_INTENT`, `UNSUPPORTED_EXECUTION_STYLE`, `UNIMPLEMENTED_POLICY`, `NO_POSITION`, `ACTIVE_SELL_RESERVATION`, `EXPOSURE_LIMIT`, `BELOW_MIN_ORDER_SIZE`, `UNPLANNABLE`, `ORDER_REJECTED`, `EXECUTION_FAILED`.

`BELOW_MIN_ORDER_SIZE` means the position is smaller than the market's minimum order size, so no order can be placed for it. It is not a permanent failure: executiond keeps the shares and retries as the lane grows, and takes the residual with a FAK if the bid reaches the close's limit price before settlement.

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

```json
{
  "schema_version": "execution.v1",
  "unique_tag": "late-gap",
  "strategy": "late-gap",
  "condition_id": "0xcondition",
  "asset_id": "12345",
  "outcome": "Up",
  "mode": "CANCEL_OPEN"
}
```
