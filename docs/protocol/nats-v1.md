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

## Subjects

| Subject | Direction | Payload | Meaning |
| --- | --- | --- | --- |
| `strategy.execution.intent` | strategy -> executiond | `ExecutionIntent` | Submit one order intent. |
| `pmm.market.quotes` | market data -> executiond | `marketquotes.Snapshot` | Latest condition quote snapshot cached for execution tactics. |
| `execution.intent.ack` | executiond -> strategy | `ExecutionIntentAck` | Validation, submission, or terminal outcome. |
| `execution.order.event` | executiond -> observers | `ExecutionOrderEvent` | Durable order-state transition observed by executiond. |
| `position.features.<condition_id>.<token_id>` | executiond -> strategy | `PositionFeature` | Latest best-effort position snapshot. |

Subject tokens must not be empty or include the NATS wildcards `*` or `>`.

## ExecutionIntent

Required fields are `schema_version`, `intent_id`, `idempotency_key`,
`strategy`, `kind`, `condition_id`, `token_id`, `outcome`, `side`,
`target_shares`, `limit_price`, and `time_in_force`. An intent also needs
either a future `expires_at` or a positive `policy.complete_within_ms`.

| Field | Values and rules |
| --- | --- |
| `kind` | `OPEN` or `CLOSE`; `CLOSE` must use `SELL`. |
| `side` | `BUY` or `SELL`. |
| `target_shares` | Positive decimal string. |
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
| `policy.reprice_interval_ms` | Optional minimum interval between active-child reprice decisions. |
| `policy.max_reprices` | Optional maximum replacement count. |
| `policy.quote_max_age_ms` | Optional maximum age for quote snapshots used by tactics. |
| `policy.post_only_cross_retry` | Optional marker for post-only cross retry behavior. |
| `policy.soft_close_after_ms` | Optional elapsed-time threshold for soft close planning. |
| `policy.force_close_after_ms` | Optional elapsed-time threshold for force/aggressive close planning. |
| `policy.cancel_replace_timeout_ms` | Optional timeout budget for cancel-before-replace flow. |

`intent_id` is the durable idempotency identity. Reusing it resumes a signed
order if necessary and does not create a second child order in the current
runtime. Advanced tactic fields are accepted and validated, and quote snapshots
are used to plan the initial child order price, post-only flag, and
time-in-force for `MAKER_POST_ONLY`, `TAKER_AGGRESSIVE`, and `AUTO` styles. The
live executor still creates one child order only. It does not yet perform
cancel-replace, post-only crossing retry, price-drift repricing after submit, or
soft/force-close lifecycle execution.

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
  "target_shares": "12.5",
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
  "target_shares": "12.5",
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
    "reprice_interval_ms": 250,
    "max_reprices": 3,
    "quote_max_age_ms": 500,
    "post_only_cross_retry": true
  }
}
```

## Output messages

`ExecutionIntentAck.status` is one of `ACCEPTED`, `REJECTED`, `COMPLETED`,
`PARTIAL`, `EXPIRED`, or `FAILED`. Rejections use stable reason codes where
available: `INVALID_INTENT`, `UNSUPPORTED_EXECUTION_STYLE`, `NO_POSITION`, and
`ORDER_REJECTED`.

`ExecutionOrderEvent.state` is the persisted runtime state. Consumers should
treat it as an observational event rather than command an order from it.

`PositionFeature.seq` is process-local and resets after an `executiond`
restart. For durable change detection, use the per-position
`source_revision`; position feature messages are snapshots and later frames
supersede earlier frames.