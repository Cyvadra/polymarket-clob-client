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
| `policy.style` | Omitted or `LIMIT`. Other styles are rejected with `UNSUPPORTED_EXECUTION_STYLE`. |
| `policy.complete_within_ms` | Optional positive execution deadline. |
| `policy.cancel_timeout_ms` | Optional cancellation timeout. |
| `policy.max_feature_age_ms` | Optional maximum age of `feature_completed_at`. |

`intent_id` is the durable idempotency identity. Reusing it resumes a signed
order if necessary and does not create a second child order. The current
runtime creates one child order only. It does not implement post-only retry,
taker repricing, price drift controls, or soft/force-close tactics.

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