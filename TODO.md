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

- [x] Deploy and check the overrun warning and the cancel p90. Verified on
  `executiond` (aws-london-light's wallet; `executiond-london` ran no
  ladders) from the Quote* lanes, 13:51-17:25 UTC: no overrun warning; batched
  cancel latency n=138 p50 84ms, p90 181ms, max 271ms (was 66/533/1444ms);
  cancels went out in batches of 3 (34), 2 (15), 1 (6).
- [x] Two instances on one host: each needs its own `EXECUTION_METRICS_ADDR`
  (`executiond` 9108, `executiond-london` 9109). A bind clash no longer exits
  the daemon, and `deploy.sh` refuses a clashing port (2807b7c).
- [x] Batch `/orders` place and cancel: one call per ladder (≤15 orders),
  404/405 falls back to single calls. Probed live 2026-11-09
  (`cmd/batchprobe`): a refused order in a batch answers `success=true` with
  only `errorMsg` set, so `BatchOrderError` reads any `errorMsg` as a refusal.
- [x] Prometheus `/metrics` on `EXECUTION_METRICS_ADDR` (default
  `127.0.0.1:9108`; pmm uses 9090-9096/9099): `executiond_cancel_seconds`,
  `executiond_loop_pass_seconds`, `executiond_order_http_*`, prepost, keep-warm.
- [x] Ladder POST ttfb rose about +100ms per child (186, 288, 393ms) when
  children were separate POSTs. Gone with batching: 51 `POST /orders` on
  2026-11-09, ttfb flat across batch sizes (2 orders about 40-300ms, 3 orders
  about 40-420ms; single `POST /order` p50 131ms, p90 245ms). No 404/405
  fallbacks.
- [ ] Slow first quote placement: 5 batch POSTs took 1.0-2.3s ttfb (14:28:01,
  15:28:53, 15:39:15, 16:13:59, 16:43:30 UTC), each the first placement of a
  new quote lane, on a reused connection (no connect or TLS time), so the
  delay is on the exchange's side. It costs 1-2s of FIFO queue position on a
  lane's first quote. Check whether it tracks the first order per market or
  token (exchange-side lookup) and whether a pre-warm call could absorb it.

## Reposting (2026-11-09)

- [ ] Check that 25 ms reposts are not throttled (order POSTs skip the client's
  rate limiter). Not exercised yet: no 429s on 2026-11-09, but only 3
  post-only crosses and about 51 quote re-statements in 3.5h. Check together
  with the requote-rate item under "Quote ladders" once FairDeltaQuote runs.
