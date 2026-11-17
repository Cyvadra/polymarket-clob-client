# TODO

Open items restored from before 5d6b78b; the done ones stay in git history.

## Quote ladders for pmm (requested 2026-11-09)

- [ ] Live checks before pmm enables FairDeltaQuote (pmm `NOTICE.md`):
  - On `executiond-london`, publish two quotes that share a level. In
    `order_events`, check that no `CANCEL_REQUESTED` and no new child appear
    at the shared price.
  - Partial evidence from the 2026-11-09 Quote/ObiQuote lanes (no quote lanes
    since 2026-11-10): the 5 `quote requote` cancels had no new child at the
    same price within 5 s. All 5 same-price reposts were a `quote empty`
    cancel followed by the quote returning ~0.1-1 s later (queue position
    lost). A two-quote shared level wasn't directly observed.
  - [x] Post-only cross wording holds for batch posts: intent 2c0bdfb4 posted
    3 levels in one batch on 2026-11-09 15:09:11, and each got
    `POST_ONLY_CROSS` "invalid post-only order: order crosses book".
  - Check `executiond_quote_fill_to_cancel_seconds` after the first real
    fills. It bounds how often a sweep fills two levels. One sample so far
    (2026-11-09 15:39:15, intent 12e11f76): fill seen to cancel requested
    727 ms, cancel answered 860 ms after the fill. Most of that is before the
    request goes out, so look there first.
  - The requote rate: about 60 level cancels per quoted market in pmm's
    replay of 2026-11-01. Compare `executiond_quote_levels_total` with the
    exchange's limits and `executiond_loop_pass_overruns_total`.
  - Restart with levels resting. They should be adopted, the deadline should
    still apply, and an owed result should be published once.

## Slow first quote placement (found 2026-11-09, aws-london-light)

- [ ] 5 batch POSTs took 1.0-2.3s ttfb (14:28:01, 15:28:53, 15:39:15,
  16:13:59, 16:43:30 UTC), each the first placement of a new quote lane, on a
  reused connection (no connect or TLS time), so the delay is on the
  exchange's side. It costs 1-2s of FIFO queue position on a lane's first
  quote. Check whether it tracks the first order per market or token
  (exchange-side lookup) and whether a pre-warm call could absorb it.
- [ ] Ladder POST ttfb rises about +100ms per child (186, 288, 393ms). Check
  for per-account throttling or a proxy queue; concurrent cancels might
  worsen it.

## Reposting (2026-11-09)

- [ ] Check that 25 ms reposts are not throttled (order POSTs skip the client's
  rate limiter). Not exercised yet: no 429s on 2026-11-09, but only 3
  post-only crosses and about 51 quote re-statements in 3.5h. Check together
  with the requote-rate item under "Quote ladders" once FairDeltaQuote runs.
