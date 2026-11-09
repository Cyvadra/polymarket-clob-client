# TODO

## Deadline pass stalls on serial ladder cancels (found 2026-11-09, aws-london-light)

- **Symptom:** `deadline pass took 1.0-1.4s, over 500ms for a 50ms interval`
  (06:03, 06:13, 06:29 UTC). Each coincides with a 3-level ladder expiring.
- **Cause:** `checkWatched` cancelled due orders one by one; each cancel is a
  blocking exchange call. Postgres is not the cost.
- **Cancel latency** (`CANCEL_REQUESTED` to `CANCEL_ACCEPTED`, 3 days, n=47):
  p50 66ms, p90 533ms, max 1444ms. No metric exists; rebuilt from `order_events`.
- **Impact:** while stalled, no other order's deadline or drift is checked; a
  child can rest about 1s past its deadline.

- [ ] Deploy it to `executiond-london`; check for the overrun warning and the cancel p90.
- [ ] Explore Polymarket's batch `/orders` cancel: one call per ladder.
- [ ] Add per-cancel and per-pass latency metrics.
- [ ] Ladder POST ttfb rises about +100ms per child (186, 288, 393ms). Check for per-account throttling or a proxy queue; concurrent cancels might worsen it.

## Reposting (2026-11-09)

- [ ] Check that 25 ms reposts are not throttled (order POSTs skip the client's
  rate limiter); see the POST ttfb item above.
