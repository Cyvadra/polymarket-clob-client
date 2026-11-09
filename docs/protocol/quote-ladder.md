# Quote ladders: what executiond needs to implement

Status: **to implement** (requested 2026-11-09). pmm already sends and reads this
contract, and its backtest venue models it. The pmm-side definition is
`pmm/internal/execution/protocol.go` (`QuoteRequest`, `QuoteResult`,
`SubjectQuoteRequest`, `SubjectQuoteResult`). Once this is built, move the
contract into [nats-v1.md](nats-v1.md) and `internal/execution/protocol/types.go`,
and delete this file.

## Why

pmm's `ladder_buy` sent one `MULTI_LEVEL_LIMIT` OPEN that lived for
`entry_timeout` (1.3s in production's FairDelta ladder). Then it was cancelled
whole, and the next tick posted a fresh ladder. A level whose price had not
moved still went to the back of Polymarket's FIFO queue every 1.3s, which hurts
the fill rate. It also produced a constant place/cancel load, which is behind
the deadline-pass stalls in [TODO.md](../../TODO.md).

pmm now makes a market instead. It says **which cents it wants a buy resting
at**, and keeps saying it as the market moves. executiond keeps every order
whose price is still wanted, so that order holds its queue place. It cancels
the orders no longer wanted and posts the new ones. pmm no longer sends
`MULTI_LEVEL_LIMIT`.

## Subjects

| Subject | Direction | Payload |
| --- | --- | --- |
| `strategy.execution.quote` | strategy -> executiond | `ExecutionQuoteRequest` |
| `execution.quote.result` | executiond -> strategy | `ExecutionQuoteResult` |

Allow both under `EXECUTION_ALLOWED_STRATEGIES`, as for opens.

## ExecutionQuoteRequest

```json
{
  "schema_version": "execution.v1",
  "request_id": "e1a2-1234",
  "unique_tag": "QuoteVUp:btc-updown-5m-1762650000",
  "strategy": "QuoteVUp",
  "event_slug": "btc-updown-5m-1762650000",
  "condition_id": "0xabc...",
  "token_id": "1234...",
  "outcome": "Up",
  "side": "BUY",
  "seq": 1762650123456789000,
  "levels": ["0.52", "0.51", "0.50"],
  "target_usd": "5.00",
  "post_only": true,
  "expires_at": "2026-11-09T01:02:06Z",
  "signal_price": "0.5350",
  "max_signal_drift": "0.0300",
  "feature_seq": 812345,
  "feature_completed_at": "2026-11-09T01:02:03Z",
  "created_at": "2026-11-09T01:02:03.120Z",
  "trigger": "tick",
  "tick_visible_at": "2026-11-09T01:02:03.100Z"
}
```

| Field | Rules |
| --- | --- |
| `unique_tag`, `condition_id`, `token_id` | The **quote lane**. Every rule below is per lane. |
| `side` | Always `BUY`. Anything else: drop the request. |
| `seq` | int64. pmm stamps it from its clock in nanoseconds, so it rises across pmm restarts. **Apply a request only if its `seq` is greater than the last `seq` applied on the lane**; otherwise drop it silently. This makes a late, duplicated or reordered NATS message harmless. |
| `levels` | Whole-cent decimal strings, highest first, distinct, at most `EXECUTION_MAX_LADDER_LEVELS` (pmm sends at most 20). **May be empty**, which means "nothing should rest". A price that is not a whole cent in `(0, 1)` makes the whole request invalid: drop it, keep the previous state. |
| `target_usd` / `target_equity_fraction` | Exactly one, as for an open. The size of **each** level (flat: one level is the full target, as with `MULTI_LEVEL_LIMIT`). Absent when `levels` is empty. |
| `post_only` | Always `true`. Every level is a post-only GTC buy. |
| `expires_at` | Required. pmm sets it 3s after `created_at` and re-sends a non-empty quote at least once a second, so it moves ahead on every request. |
| `signal_price`, `max_signal_drift` | Optional, set together, same meaning as on an open. The bound applies to every level. `signal_price` is the mid when the quote started, and stays the same across the requests of one quote. |
| `request_id` | Opaque. Store it on the levels that request posted, and echo the one whose level filled on the result. |
| provenance (`feature_seq`, `feature_completed_at`, `created_at`, `trigger`, `tick_visible_at`) | Store for audit, as for opens. |

## What executiond does with a request

Keep, per lane, the last applied `seq`, the set of **working levels** (the
price, its child order, and its state: posting, resting, parked for cross
retry, cancelling), the lane's deadline, and a `done` flag.

1. **Refuse when done.** If the lane is `done` (it has filled, see below),
   drop the request and change nothing. That includes an empty request.
2. **Order.** If `seq` is not greater than the last applied, drop it. Otherwise
   record it.
3. **Diff by price.** Let `want` be the request's levels and `have` the working
   levels.
   - **In both:** leave the order alone. Do not cancel and re-post it, and do
     not amend it. Keeping its queue place is the whole point. Move its
     deadline to the new `expires_at`. A level parked for cross retry stays
     parked and keeps retrying.
   - **In `have` only:** cancel it.
   - **In `want` only:** post a new post-only GTC buy at that price for one
     level's size, with the same reservation, sizing and minimum-size rules as
     a `MULTI_LEVEL_LIMIT` level. If it is refused because it would cross the
     book, park it and repost every 25ms until it rests, fills, leaves `want`,
     or the lane ends (the same retry as today's ladder).
   - **Empty `want`:** cancel every working level. The lane is not `done`, and
     a later request may quote again.
4. **Deadline.** When the clock passes the lane's deadline with no newer
   request, cancel every working level. This is the safety net for a pmm that
   stopped; pmm never relies on it otherwise. It ends nothing permanently: a
   later request quotes again.
5. **Drift.** Check `max_signal_drift` before signing each new level, and every
   deadline pass for resting levels, as for opens. A level that breaches is
   cancelled (or not posted). Unlike an open, the lane continues, so a later
   request may post levels again. No result is published for a drift cancel.

Process each lane's requests in order, and do not let one lane's exchange calls
block another's. A request that arrives while the previous one's posts or
cancels are still in flight is applied against the state they leave: queue it
behind them, or merge it so only the newest `want` is applied. Either is
correct, because only the newest `want` matters.

## The first fill ends the quote

When **any** level reports a fill, partial or whole, through the order or
trade stream, or a submission that matched on arrival:

1. Mark the lane `done` at once, before any exchange call, so that a request
   arriving in between is refused.
2. Cancel every other working level, and the unfilled rest of the level that
   filled. Stop all cross retries. **Use the batch cancel** that ladders already
   use (00641a8, one `/orders` call): the measured single-cancel latency is p50
   66ms and p90 533ms (TODO.md), and each millisecond a sibling rests is a
   chance for a sweep to fill a second level. A requote's cancels and posts can
   batch the same way.
3. Publish **one** `ExecutionQuoteResult` (below) as soon as the first fill is
   known. Do not wait for the cancels to finish. If more fills land before the
   cancels do, they reach pmm through `position.features.*`. Do not publish a
   second result.
4. Book the position as for an open: one lot (`open_lots` counts the whole
   quote as one), with the entry price from the fills. `position.features.*`
   follows as usual.

`done` lasts for the lane: that `unique_tag` + `condition_id` + `token_id`.
It may be dropped once the market settles.

## ExecutionQuoteResult

```json
{
  "schema_version": "execution.v1",
  "request_id": "e1a2-1234",
  "unique_tag": "QuoteVUp:btc-updown-5m-1762650000",
  "strategy": "QuoteVUp",
  "wallet_address": "0x...",
  "condition_id": "0xabc...",
  "token_id": "1234...",
  "outcome": "Up",
  "side": "BUY",
  "status": "FILLED",
  "price": "0.51",
  "filled_shares": 9.8,
  "average_price": 0.51,
  "occurred_at": "2026-11-09T01:02:04.310Z"
}
```

- `status` is always `FILLED`. Nothing else is published on this subject: an
  emptied, expired, drifted or refused quote is silent, because pmm already
  knows its own requests and reads fills from position snapshots.
- `price` is the filled level. `filled_shares` and `average_price` are what is
  known when the result is published. pmm counts the lot once and does not
  depend on these being final. `request_id` is the request that posted the
  filled level.
- Exactly once per lane per wallet, including across restarts: persist that it
  is owed, as ladders persist `ladder_phase`.

## Several wallets

pmm sends a quote once, and every allowed instance applies it for its own
wallet. A fill in wallet A makes A `done`, but B's levels are still working.
pmm handles this: on the first `FILLED` it receives, it sends an **empty quote**
with a higher `seq`. B then cancels its levels, and A drops it because A is
`done`. executiond needs nothing extra for this, but it must apply an empty
quote at once, without waiting for the deadline.

## Restarts

- Persist per lane: the last `seq`, the deadline, `done`, the working levels
  and their child orders, and whether a result is owed.
- On restart, adopt resting levels and keep honouring the deadline: pmm
  re-states the quote within a second, and the diff then keeps or cancels what
  was adopted. A level whose fill arrived while the process was down still
  ends the quote and still produces the owed result.

## What pmm no longer sends

`policy.style: MULTI_LEVEL_LIMIT`, `bottom_price` and `scaling_mode` are gone
from pmm. Its quote lanes also never send `CANCEL_OPEN`. You may keep the ladder
code until no pmm build that sends it is deployed, but nothing new depends on it.
`FORCE_CLOSE` on a quote lane should still cancel any working levels, as it
does for a working open.

## Checks before production

pmm keeps the FairDeltaQuote lanes disabled until these hold (pmm `NOTICE.md`):

- [ ] Both subjects implemented and allowed; `nats-v1.md` and `types.go`
      updated; this file deleted.
- [ ] A requote that keeps a level makes **no** exchange call for that level
      (check `order_events`: no `CANCEL_REQUESTED` or new child at that price
      between the two requests).
- [ ] Out-of-order `seq` is dropped (test with two requests published in
      reverse order).
- [ ] An empty quote cancels everything; a lapsed `expires_at` cancels
      everything; neither sets `done`.
- [ ] First fill: `done` is set before any call, one result is published, and
      the siblings are cancelled by one batch call. Record fill-to-cancel-ack
      latency as a metric; it bounds how often a sweep fills two levels.
- [ ] The post-only cross wording (confirmed in 39c6357) also holds for a
      level posted through the batch `/orders` call (per-order `errorMsg`).
- [ ] Requote rate: about 60 level cancels per quoted market in pmm's replay
      of 2026-11-01. Check that rate against the exchange's limits and the
      deadline pass, and add per-lane counters of posts and cancels.
- [ ] Restart with levels resting: they are adopted, the deadline still
      applies, and an owed result is still published once.
