# TODO

## Quote ladders for pmm (requested 2026-11-09)

- [x] Implement `strategy.execution.quote` / `execution.quote.result`. The
  contract is in [nats-v1.md](docs/protocol/nats-v1.md#quote-ladders) and the
  code in `pkg/executor/quote.go`. Unit tests cover: a kept level makes no
  exchange call, a stale seq is dropped, empty quotes and deadlines cancel
  without setting done, the first fill makes one result and one batch cancel,
  a parked level is reposted, force close, and restart adoption with an owed
  result.
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

## Deadline pass stalls on serial ladder cancels (found 2026-11-09, aws-london-light)

- **Symptom:** `deadline pass took 1.0-1.4s, over 500ms for a 50ms interval`
  (06:03, 06:13, 06:29 UTC). Each coincides with a 3-level ladder expiring.
- **Cause:** `checkWatched` cancelled due orders one by one; each cancel is a
  blocking exchange call. Postgres is not the cost.
- **Cancel latency** (`CANCEL_REQUESTED` to `CANCEL_ACCEPTED`, 3 days, n=47):
  p50 66ms, p90 533ms, max 1444ms. No metric exists; rebuilt from `order_events`.
- **Impact:** while stalled, no other order's deadline or drift is checked; a
  child can rest about 1s past its deadline.

- [x] Deploy it to `executiond-london`; check for the overrun warning and the cancel p90.
- [x] Batch `/orders` place and cancel: one call per ladder (≤15 orders),
  404/405 falls back to single calls. Probed live 2026-11-09
  (`cmd/batchprobe`): a refused order in a batch answers `success=true` with
  only `errorMsg` set, so `BatchOrderError` reads any `errorMsg` as a refusal.
- [x] Prometheus `/metrics` on `EXECUTION_METRICS_ADDR` (default
  `127.0.0.1:9108`; pmm uses 9090-9096/9099): `executiond_cancel_seconds`,
  `executiond_loop_pass_seconds`, `executiond_order_http_*`, prepost, keep-warm.
- [ ] Ladder POST ttfb rises about +100ms per child (186, 288, 393ms). Check for per-account throttling or a proxy queue; concurrent cancels might worsen it.

## Reposting (2026-11-09)

- [ ] Check that 25 ms reposts are not throttled (order POSTs skip the client's
  rate limiter); see the POST ttfb item above.
