# Execution Runtime TODO

This file records deliberately unfinished execution work. It is not a claim
that these behaviors are already implemented by `executiond`.

## Market-Driven Execution Tactics

- Implement a per-intent, multi-child-order worker for post-only cross retry,
  taker repricing, price-drift rejection, and soft-close to force-close
  escalation.
- Persist each child with an incremented `child_sequence`, including its price,
  cancellation cause, and replacement relationship.
- Consume a bounded, timestamped order-book snapshot as a dedicated runtime
  input. Do not make the executor fetch or subscribe to pmm features.

Why this is not implemented now: the current contract carries
`style`, `max_reprices`, `reprice_step`, `max_price_drift`, and reprice delay,
but `executiond` has no owned, authoritative market-data source. Implementing
these tactics from stale REST reads would turn an execution policy into
unbounded side effects with unknown market freshness. The strategy must provide
an execution-safe market snapshot contract, or executiond must own a dedicated
CLOB book feed, before this worker is added.

## Risk Controls

- Add a durable wallet budget and allowance view.
- Enforce per-strategy, per-market, per-token, and global notional limits.
- Validate CLOB minimum order size and tick alignment before reservation.
- Enforce monotonic feature sequence per strategy/market when the strategy
  elects to use feature sequencing.

Why this is not implemented now: there is no risk configuration schema, no
wallet-level budget table, and no policy for handling positions created outside
this service. Adding arbitrary fixed limits would silently change strategy
behavior. Define those limits and their ownership first, then make reservation
the atomic risk boundary.

## Reconnect and External Activity Reconciliation

- On user-stream reconnect, backfill recent account trades and open orders
  before accepting new stream updates.
- Persist a CLOB trade cursor or high-water mark.
- Record fills for unknown external orders as explicit external activity, then
  reconcile them with the durable position view instead of dropping them.
- Compare durable inventory with an authoritative wallet/token balance and
  surface divergence as an execution health event.

Why this is not implemented now: CLOB account APIs in the current client expose
paginated account reads but no persisted cursor/replay contract. A naive
`AllTrades` replay would repeatedly scan the whole account and can bind an
unrelated historical trade. The cursor, retention period, and policy for manual
or other-process trading must be defined before backfill is safe.

## Settlement and Redemption

- Add `settled` and `redeemed` position states.
- Reconcile market resolution and redeemed collateral after event settlement.
- Publish a final position feature when a market position becomes empty through
  settlement rather than an order fill.

Why this is not implemented now: market resolution and on-chain redemption are
outside the CLOB order lifecycle and require a chain/indexer authority. The
current service deliberately reports only trade-derived execution inventory.

## Single-Wallet Ownership and Operations

- Acquire a PostgreSQL advisory lease keyed by wallet before starting execution.
- Refuse startup if another `executiond` instance owns the wallet.
- Add health/readiness endpoints and metrics for NATS, CLOB, user stream,
  reconciliation lag, stale signed orders, failed settlements, and position
  divergence.
- Add PostgreSQL-backed integration tests for concurrent reservations, fill
  rollback, terminal reservation release, and crash recovery.

Why this is not implemented now: `executiond` currently has no configured
wallet identity independent of CLOB credentials, and the test environment does
not provision PostgreSQL. Both need explicit deployment configuration rather
than process-local assumptions.

## Current Guarantees

- Signed orders are persisted before submission and `SIGNED` orders are
  resumed after a restart.
- Definitive CLOB submission rejections are `REJECTED`; unknown submission
  outcomes are reconciled rather than blindly resubmitted.
- An unresolved submission eventually reaches `FAILED`, releasing its
  reservation.
- Authenticated order and trade events drive durable order state and position
  accounting.
- Taker-buy share fees and later permanent trade failure are accounted for
  consistently.
- Strategies receive acceptance/rejection and terminal intent acknowledgements,
  plus best-effort position features.