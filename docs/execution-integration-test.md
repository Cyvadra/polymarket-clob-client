# executiontest

`cmd/executiontest` is a real-money, NATS-only black-box test for a running
`executiond`. It connects to NATS, sends protocol requests, and uses only NATS
messages (results, order events, position features, position queries, and quote
snapshots) as evidence. It does not import or call the executor, store,
reconciler, account feed, CLOB client, or any private Polymarket API.

A run drives one trade through its whole life on a fresh lane
(`<strategy>-<unix-nanos>`), as a sequence of phases. Each phase writes its own
row to the report:

| Phase | What it does |
| --- | --- |
| `baseline-flat` | Queries the supplied condition ID and requires no visible position for the target asset. Positions on other assets in the same condition are reported but do not fail the phase. |
| `open` | Sends one `LIMIT`/`GTC` BUY at the explicit target amount and buy limit, and waits for a terminal `execution.open.result` with filled shares. |
| `open-fills-reconcile` | Sums `matched_shares` from this lane's `execution.order.event` messages and compares the total with the open result. |
| `position-open` | Waits for the lane's position to become visible, then checks `position_size` against the filled shares and that nothing is unexpectedly reserved. |
| `hold` | Optional dwell (`--hold`); checks the position survives and `seconds_since_entry` advances. |
| `close` | Exits the lane. See [Close modes](#close-modes). |
| `close-fills-reconcile` | Compares the shares matched by every close leg with the shares opened. |
| `position-flat` | Waits for the lane to disappear from the position query. |
| `pnl-summary` | Gross, pre-fee P&L from the open and close results, when both legs have a known price. |
| `no-residual-orders` | Requires every order seen for the lane to have last been observed in a terminal state. |

A deferred `cleanup` step runs after the phases. If the lane is already
confirmed flat it sends nothing. Otherwise it sends a `FORCE_CLOSE` and waits
for the lane to disappear, so an interrupted or failed run still tries to
leave no position behind.

Every run writes an incrementally synced Markdown report. It includes the NATS
evidence, per-phase outcomes, and a final summary. The console prints its path
and the subjects observed during the run.

## Prerequisites

- A dedicated wallet, PostgreSQL database, and NATS account with exactly one
  running `executiond`.
- No other strategy or manual trading activity on the supplied asset while the
  tool runs.
- The caller has independently verified the condition ID, asset/token ID,
  outcome label, and the buy limit price. This tool does not resolve a slug or
  discover outcome tokens, and it has no confirmation prompt.
- The running daemon has authenticated CLOB credentials and sufficient funds.
- For the quote-crossing check in the limit-close scenario, the tool must be
  able to observe `pmm.market.quotes`; without quotes that check is only
  informational.

Core NATS provides at-most-once delivery. The tool never retries a BUY after a
publish or result timeout because the daemon may already have accepted it.

## Run

```sh
go run ./cmd/executiontest \
  --condition-id "0x..." \
  --asset-id "..." \
  --outcome "Up" \
  --target-usd "1.00" \
  --buy-limit "0.42"
```

Those five flags are the entire required set. `--token-id` is an alias for
`--asset-id`. Without `--sell-limit` the run exits with a `FORCE_CLOSE`.

To exercise a limit-close first, add a sell limit:

```sh
go run ./cmd/executiontest \
  --condition-id "0x..." --asset-id "..." --outcome "Up" \
  --target-usd "1.00" --buy-limit "0.42" \
  --sell-limit "0.55" --hold 30s
```

The NATS URL is not a flag. It is read from `EXECUTION_NATS_URL`, falling back
to `nats://127.0.0.1:4222` — the same resolution `executiond` uses, so a shell
that can start the daemon can run the test without repeating the URL.

### Optional flags

All have usable defaults.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--sell-limit` | empty | SELL limit price in (0,1) for the limit-close scenario. Empty skips it and closes with `FORCE_CLOSE`. |
| `--close-mode` | `auto` | `auto`, `limit`, or `force`. See [Close modes](#close-modes). |
| `--hold` | `0` | Dwell between the confirmed open and the close. `0` skips the hold phase; it is uncapped, so only Ctrl-C interrupts a long hold. |
| `--negative` | off | Run the invalid-schema rejection probe before the lifecycle. See [Negative probe](#negative-probe). |
| `--strategy` | `executiontest` | Strategy and lane namespace. |
| `--report-dir` | `reports/executiontest` | Directory for Markdown reports. |
| `--case-timeout` | `5m` | Completion deadline sent to `executiond` as `complete_within_ms` (and as `expires_at`), and the base wait for a terminal open or close result. |
| `--result-grace` | `30s` | Extra wait beyond `--case-timeout` for a terminal result, covering `executiond`'s own cancel-and-report lag once its deadline passes. |
| `--position-timeout` | `90s` | Longest wait for a position to reach the expected state. |
| `--query-timeout` | `15s` | Longest wait for a single position query reply. |
| `--cleanup-timeout` | `90s` | Longest time for the deferred force-close backstop. |

The tool creates report directories with mode `0700` and report files with mode
`0600`.

## Close modes

- **No `--sell-limit`, or `--close-mode force`:** sends `FORCE_CLOSE` and
  confirms the exit through the position query, because `FORCE_CLOSE` never
  emits `execution.close.result`.
- **`--sell-limit` with `--close-mode limit`:** sends `LIMIT_CLOSE` at the sell
  limit and never falls back. Before sending, it records whether the limit
  crosses the best bid seen on `pmm.market.quotes` (an immediate taker sell) or
  rests until the bid reaches it.
- **`--sell-limit` with `--close-mode auto`:** sends `LIMIT_CLOSE`; if that does
  not fully exit the lane (no fill, or a partial fill reported as `SUCCEEDED`),
  falls back to `FORCE_CLOSE`. After a fallback the total exit quantity and
  price are unknown, so `pnl-summary` is skipped.

## Negative probe

With `--negative`, the run first publishes an open request with an unsupported
`schema_version` and requires an `INVALID_INTENT` failure result. The probe
cannot place an order. If it fails, the run stops before any real BUY is sent.
Without the flag the probe does not run.

## No confirmation prompt

There is no `--confirm-real-money` gate. A syntactically valid invocation
submits a real order immediately. Local validation still rejects a missing
condition ID, asset ID or outcome, a non-positive `--target-usd`, a
`--buy-limit` or `--sell-limit` outside `(0,1)`, an unknown `--close-mode`, and
non-positive timeouts — but it cannot catch a correctly-formed wrong value, so
a mistyped asset ID or target amount will reach the exchange. The runner's own
preflight remains: it aborts before buying if a position is already visible
for the supplied asset.

There is no per-run budget flag. The effective exposure cap is
`EXECUTION_MAX_OPEN_BUY_NOTIONAL_USD` on the daemon, which is enforced inside
the reservation transaction. Set it if you want a hard ceiling on this run.

## FORCE_CLOSE risk

`FORCE_CLOSE` (used for the default close, the `auto` fallback, and cleanup) is
implemented by the daemon as an internal `SELL 0.01 FAK`. It may incur a severe
exit price, it does not emit `execution.close.result`, and it may fail to exit
if it cannot match. The tool therefore considers the exit confirmed only when
its NATS position query no longer shows the lane. It does not prove that the
account has no unrelated orders, and users must reconcile the report's time,
token identity, and any observed IDs with their account's trade history.

## Evidence boundaries

The report marks missing evidence as `INCONCLUSIVE`, not successful. Phase
outcomes are `PASS`, `FAIL`, `SKIP`, `INCONCLUSIVE`, or `INFO`. An accounting
mismatch is recorded as `FAIL` but does not stop the run, so the position it
opened is still closed.

`execution.order.event` carries the lane's `unique_tag`, so the tool filters the
shared subject to its own lane and attributes events to orders by intent ID.
The fills reconciliation totals each order's cumulative `matched_shares` once
and accepts a 1% difference (at least `1e-6` shares); the position check
accepts 2%. If no order event for the lane is observed, those phases are
`INCONCLUSIVE` or `SKIP` rather than passing.

The tool cannot prove order signatures, exchange fill price, settlement,
persistence atomicity, CLOB account balance, user-stream health, reconciler
behavior, or the absence of all account orders. The P&L figure is gross and
pre-fee, computed from the prices on the execution results; it does not read
the fees `executiond` records per fill, so it can differ from the wallet's
actual result on markets that charge fees.

Future scenario work can add explicit validation, execution style, expiry,
replacement, lane-isolation, and quote-dependent coverage while preserving the
same NATS-only boundary.
