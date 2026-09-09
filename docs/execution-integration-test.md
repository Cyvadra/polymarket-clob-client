# executiontest

`cmd/executiontest` is a real-money, NATS-only black-box smoke test for a
running `executiond`. It connects to NATS, sends protocol requests, and uses
only NATS results and position queries as evidence. It does not import or call
the executor, store, reconciler, account feed, CLOB client, or any private
Polymarket API.

The initial implementation exercises this bounded sequence:

1. Query the supplied condition ID and require no currently visible position.
2. Submit one `LIMIT`/`GTC` BUY at the explicit target amount and buy limit.
3. Wait for a successful `execution.open.result` and a NATS-visible position.
4. Send a `FORCE_CLOSE` cleanup request and wait for the position query to no
   longer show that run's lane.

Every run writes an incrementally synced Markdown report. It includes the NATS
evidence, case outcomes, and a final summary. The console prints its path and
the subjects observed during the run.

## Prerequisites

- A dedicated wallet, PostgreSQL database, and NATS account with exactly one
  running `executiond`.
- No other strategy or manual trading activity on the supplied asset while the
  tool runs.
- The caller has independently verified the condition ID, asset/token ID,
  outcome label, and the buy limit price. This tool does not resolve a slug or
  discover outcome tokens, and it has no confirmation prompt.
- The running daemon has authenticated CLOB credentials and sufficient funds.

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
`--asset-id`.

The NATS URL is not a flag. It is read from `EXECUTION_NATS_URL`, falling back
to `nats://127.0.0.1:4222` — the same resolution `executiond` uses, so a shell
that can start the daemon can run the test without repeating the URL.

Optional flags, all with usable defaults: `--strategy` (`executiontest`),
`--report-dir` (`reports/executiontest`), `--case-timeout` (`2m`), and
`--cleanup-timeout` (`30s`).

The tool creates report directories with mode `0700` and report files with mode
`0600`.

## No confirmation prompt

There is no `--confirm-real-money` gate. A syntactically valid invocation
submits a real order immediately. Local validation still rejects a missing
condition ID, asset ID or outcome, a non-positive `--target-usd`, a
`--buy-limit` outside `(0,1)`, and non-positive timeouts — but it cannot catch a
correctly-formed wrong value, so a mistyped asset ID or target amount will
reach the exchange. The runner's own preflight checks remain: it aborts before
buying if the schema-rejection case fails or if a position is already visible
for the supplied filters.

The per-run budget flags (`--max-total-buy-usd`, `--sell-limit`) were removed
because the runner never used them; they were validated and printed but had no
effect on any order. The effective exposure cap is
`EXECUTION_MAX_OPEN_BUY_NOTIONAL_USD` on the daemon, which is enforced inside
the reservation transaction. Set it if you want a hard ceiling on this run.

## FORCE_CLOSE risk

Cleanup sends the runtime's `FORCE_CLOSE` command. The daemon implements it as
an internal `SELL 0.01 FAK`; it may incur a severe exit price, it does not emit
`execution.close.result`, and it may fail to exit if it cannot match. The tool
therefore considers cleanup successful only when its NATS position query no
longer shows the lane. It does not prove that the account has no unrelated
orders, and users must reconcile the report's time, token identity, and any
observed IDs with their account's trade history.

## Evidence boundaries

The report marks missing evidence as `INCONCLUSIVE`, not successful. It cannot
prove order signatures, exchange fill price, fees, settlement, persistence
atomicity, CLOB account balance, user-stream health, reconciler behavior, or
the absence of all account orders. `execution.order.event` includes an intent
ID but no lane or asset identity, so this first implementation records the
events without claiming deterministic attribution to the test BUY.

Future scenario work can add explicit validation, execution style, expiry,
replacement, lane-isolation, and quote-dependent coverage while preserving the
same NATS-only boundary.