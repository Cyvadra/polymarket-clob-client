# executiond NATS Protocol v1

`executiond` uses core NATS. The protocol version is `execution.v1` and all
messages on the subjects below are JSON objects. Senders should include
`schema_version`; receivers currently accept an omitted version only for the
authenticated CLOB user-stream adapter, never as a strategy integration rule.

## Delivery and deployment

- Intents use ordinary core-NATS publish/subscribe and are at-most-once. There
  is no durable consumer, replay, or transport acknowledgement.
- Run one `executiond` instance per wallet. Core NATS subscriptions are not a
  queue group, so more than one instance receives the same intent.
- A decodable intent with a non-empty `intent_id` always receives a rejection
  acknowledgement when validation fails. Malformed JSON with no usable ID can
  only be logged.
- Published acknowledgements, order events, and position features are
  best-effort. Strategies must tolerate a missing event and use their own
  deadline for the at-most-once intent decision.
- On startup and each reconciliation tick, `executiond` replays account trades
  from CLOB REST through the same idempotent fill path as the user stream.
  This repairs fills missed during WebSocket disconnects when CLOB REST exposes
  the trade.

## Subjects

| Subject | Direction | Payload | Meaning |
| --- | --- | --- | --- |
| `strategy.execution.intent` | strategy -> executiond | `ExecutionIntent` | Submit one order intent. |
| `pmm.market.quotes` | market data -> executiond | `marketquotes.Snapshot` | Latest condition quote snapshot cached for execution tactics. |
| `execution.intent.ack` | executiond -> strategy | `ExecutionIntentAck` | Validation, submission, or terminal outcome. |
| `execution.order.event` | executiond -> observers | `ExecutionOrderEvent` | Durable order-state transition observed by executiond. |
| `position.features.<condition_id>.<token_id>` | executiond -> strategy | `PositionFeature` | Latest best-effort position snapshot. |
| `strategy.execution.cancel` | strategy -> executiond | `ExecutionCancelRequest` | Cancel an intent and force close its token position. |
| `execution.cancel.ack` | executiond -> strategy | `ExecutionCancelAck` | Terminal outcome of a cancel command. |
| `strategy.execution.position.query` | strategy -> executiond (request/reply) | `PositionQueryRequest` | Query current positions; the reply is published on the request's reply subject. |

Subject tokens must not be empty or include the NATS wildcards `*` or `>`.

## ExecutionIntent

Required fields are `schema_version`, `intent_id`, `idempotency_key`,
`strategy`, `kind`, `condition_id`, `token_id`, `outcome`, `side`,
`target_usd`, `limit_price`, and `time_in_force`. An intent also needs
either a future `expires_at` or a positive `policy.complete_within_ms`.

| Field | Values and rules |
| --- | --- |
| `kind` | `OPEN` or `CLOSE`; `CLOSE` must use `SELL`. |
| `side` | `BUY` or `SELL`. |
| `target_usd` | Positive decimal string. The program calculates shares from this amount and the order price. |
| `limit_price` | Decimal string strictly between `0` and `1`. |
| `time_in_force` | `GTC`, `FOK`, `FAK`, or `GTD`. |
| `post_only` | Boolean passed to the CLOB order. |
| `policy.style` | Omitted, `LIMIT`, `MAKER_POST_ONLY`, `TAKER_AGGRESSIVE`, or `AUTO`. Omitted is treated as `LIMIT`. |
| `policy.complete_within_ms` | Optional positive execution deadline. |
| `policy.cancel_timeout_ms` | Optional cancellation timeout. |
| `policy.max_feature_age_ms` | Optional maximum age of `feature_completed_at`. |
| `policy.initial_price` | Optional decimal string used by tactic planning; defaults to `limit_price`. |
| `policy.max_price` | Required for advanced `BUY` tactics; hard ceiling for automatic repricing. |
| `policy.min_price` | Required for advanced `SELL` tactics; hard floor for automatic repricing. |
| `policy.price_step` | Optional decimal string for tactic price increments. |
| `policy.quote_offset` | Optional decimal string offset from best bid/ask for maker planning. |
| `policy.reprice_interval_ms` | Not implemented; non-zero values are rejected. |
| `policy.max_reprices` | Not implemented; non-zero values are rejected. |
| `policy.quote_max_age_ms` | Optional maximum age for quote snapshots used by tactics. |
| `policy.post_only_cross_retry` | Not implemented; true is rejected. |
| `policy.soft_close_after_ms` | Not implemented; non-zero values are rejected. |
| `policy.force_close_after_ms` | Not implemented; non-zero values are rejected. |
| `policy.cancel_replace_timeout_ms` | Not implemented; non-zero values are rejected. |

`intent_id` is the durable idempotency identity. Reusing it resumes a signed
order if necessary and does not create a second child order in the current
runtime. Quote snapshots are used to plan the initial child order price, post-only flag, and
time-in-force for `MAKER_POST_ONLY`, `TAKER_AGGRESSIVE`, and `AUTO` styles. The
live executor still creates one child order only. It does not yet perform
cancel-replace, post-only crossing retry, price-drift repricing after submit, or
soft/force-close lifecycle execution; policy fields for those behaviors are rejected.

Example:

```json
{
  "schema_version": "execution.v1",
  "intent_id": "late-gap:condition:up:42",
  "idempotency_key": "late-gap:condition:up:42",
  "strategy": "late-gap",
  "kind": "OPEN",
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

Maker post-only policy example accepted by validation and the tactic planner:

```json
{
  "schema_version": "execution.v1",
  "intent_id": "late-gap:condition:up:43",
  "idempotency_key": "late-gap:condition:up:43",
  "strategy": "late-gap",
  "kind": "OPEN",
  "condition_id": "0xcondition",
  "token_id": "12345",
  "outcome": "Up",
  "side": "BUY",
  "target_usd": "12.5",
  "limit_price": "0.42",
  "time_in_force": "GTC",
  "post_only": true,
  "expires_at": "2026-09-04T12:05:00Z",
  "policy": {
    "style": "MAKER_POST_ONLY",
    "initial_price": "0.42",
    "max_price": "0.48",
    "price_step": "0.01",
    "quote_offset": "0.01",
    "quote_max_age_ms": 500
  }
}
```

Lifecycle controls such as `reprice_interval_ms`, `max_reprices`,
`post_only_cross_retry`, `soft_close_after_ms`, `force_close_after_ms`, and
`cancel_replace_timeout_ms` are reserved for later cancel-replace execution and
are currently rejected with `UNIMPLEMENTED_POLICY` when non-zero or true.

## ExecutionCancelRequest

Cancels an intent by abandoning its position. The strategy sends one cancel per
intent it wants to stop managing.

| Field | Values and rules |
| --- | --- |
| `schema_version` | Required, `execution.v1`. |
| `intent_id` | Required. The intent to cancel. |
| `reason` | Optional free-form reason surfaced in the acknowledgement. |

Cancel handling is idempotent and atomic per `intent_id`:

- Any still-open strategy-facing child order of the intent is canceled on the
  CLOB. A child that was persisted but never submitted is marked canceled
  before submission instead of being recovered and sent.
- If the intent opened a position, `executiond` force closes the **whole
  remaining available position** for the intent's `condition_id`/`token_id` by
  submitting an internal `0.01 SELL FAK` child of the same intent. The forced
  sell is never an `execution.intent.ack`; it is reported through
  `execution.cancel.ack` and the normal order-event/position-feature stream.
- A repeated cancel for an intent whose force-close child already exists does
  nothing further: an unfilled forced close is deliberately not retried.

### Forced-close terminal semantics

A forced `0.01 SELL FAK` is terminal for strategy tracking as soon as it is
dispatched. If it does not fill (for example because there is no bid at `0.01`
near event end), `executiond` treats it as done from the strategy's point of
view: the sell reservation is released through the normal terminal order path,
no further exit is attempted, and any leftover shares await market settlement.
`executiond` does not mark the position with a durable "closed" flag; position
snapshots keep reflecting the current durable state. Strategies that sent the
cancel are expected to stop managing the token after the terminal
`execution.cancel.ack` and ignore any later position frame for it.

If the token already has another active sell (a competing sell reservation),
the forced sell is skipped and the acknowledgement reports
`ACTIVE_SELL_RESERVATION`; the in-flight sell is that token's exit.

## ExecutionCancelAck

`execution.cancel.ack` is the single terminal signal for a cancel command. It is
published best-effort; strategies should use their own timeout if they require
one. `status` is one of:

| Status | Meaning |
| --- | --- |
| `COMPLETED` | Orders canceled and, when a position existed, the force close was dispatched (fill outcome is reported via order events and position features). |
| `CANCELED` | Open order canceled; there was no position to force close. |
| `NO_POSITION` | No available position could be reserved for the forced sell. |
| `ACTIVE_SELL_RESERVATION` | Force close skipped because another sell is already active on the token. |
| `NOT_FOUND` | No execution intent exists for `intent_id`. |
| `FAILED` | The cancel could not be completed; `reason_code` is `INVALID_CANCEL` for a malformed request or `EXECUTION_FAILED` otherwise. |

Example:

```json
{
  "schema_version": "execution.v1",
  "intent_id": "late-gap:condition:up:42",
  "status": "COMPLETED",
  "reason": "open orders canceled and position force close submitted at 0.01 FAK",
  "canceled_orders": 1,
  "occurred_at": "2026-09-04T12:05:01Z"
}
```

## PositionQueryRequest

Request/reply position snapshot. Send the request on
`strategy.execution.position.query`; `executiond` publishes the response on the
request's NATS reply subject (`Msg.Reply`). Replies are one-off snapshots and
carry the same shape as the streaming `PositionFeature`; `seq` is `0`.

| Field | Values and rules |
| --- | --- |
| `schema_version` | Required, `execution.v1`. |
| `condition_id` | Optional. Restricts the reply to one condition. |
| `market_id` | Optional. Restricts the reply to one market. Both filters may be combined. |

Without a filter the reply contains every currently held position (rows with a
positive position size; empty rows are excluded).

Reply payload `PositionQueryResponse`:

```json
{
  "schema_version": "execution.v1",
  "positions": [ { "condition_id": "0xcondition", "token_id": "12345", "...": "..." } ]
}
```

## Output messages

`ExecutionIntentAck.status` is one of `ACCEPTED`, `REJECTED`, `COMPLETED`,
`PARTIAL`, `EXPIRED`, or `FAILED`. Rejections use stable reason codes where
available: `INVALID_INTENT`, `UNSUPPORTED_EXECUTION_STYLE`,
`UNIMPLEMENTED_POLICY`, `NO_POSITION`, `ACTIVE_SELL_RESERVATION`,
`DUPLICATE_INTENT`, and `ORDER_REJECTED`.

`ExecutionOrderEvent.state` is the persisted runtime state. Consumers should
treat it as an observational event rather than command an order from it.

If a submitted order has an unresolved outcome and CLOB REST returns `404`,
`executiond` first marks the order `UNKNOWN_RECONCILE`. Only after the
configured missing-order grace period does a continuing `404` become terminal
`FAILED`, which releases any active reservation.

`PositionFeature.seq` is process-local and resets after an `executiond`
restart. For durable change detection, use the per-position
`source_revision`; position feature messages are snapshots and later frames
supersede earlier frames.