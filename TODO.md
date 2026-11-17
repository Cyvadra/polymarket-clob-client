# TODO

Open items restored from before 5d6b78b; the done ones stay in git history.

## Quote ladders for pmm (requested 2026-11-09)

- [ ] Run the Postgres store tests (`go test -tags postgres
  ./pkg/store/postgres/ -run Quote`, with `EXECUTION_TEST_POSTGRES_URL`) and
  apply migration `000012_quote_lanes.sql`.
- [ ] Live checks before pmm enables FairDeltaQuote (pmm `NOTICE.md`):
  - On `executiond-london`, publish two quotes that share a level. In
    `order_events`, check that no `CANCEL_REQUESTED` and no new child appear
    at the shared price.
  - Check that the post-only cross wording (39c6357) also holds for a level
    posted through the batch `/orders` call (per-order `errorMsg`).
  - Check `executiond_quote_fill_to_cancel_seconds` after the first real
    fills. It bounds how often a sweep fills two levels.
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
