-- Equity is recorded after positions settle, not continuously. Between two
-- snapshots, the cash change that recorded trading does not explain (fills and
-- winner payouts) is taken as a deposit or withdrawal, external_flow_usd, and
-- left out of trade_index: a time-weighted return index of trading alone,
-- starting at 1, which a drawdown limit is measured on.
CREATE TABLE IF NOT EXISTS equity_snapshots (
    id BIGSERIAL PRIMARY KEY,
    taken_at TIMESTAMPTZ NOT NULL,
    reason TEXT NOT NULL,
    cash_usd NUMERIC(38, 18) NOT NULL,
    positions_usd NUMERIC(38, 18) NOT NULL,
    equity_usd NUMERIC(38, 18) NOT NULL,
    trade_cash_usd NUMERIC(38, 18) NOT NULL,
    external_flow_usd NUMERIC(38, 18) NOT NULL,
    trade_index NUMERIC(38, 18) NOT NULL,
    unmarked_positions INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS equity_snapshots_taken_at ON equity_snapshots (taken_at DESC);

-- Redeeming a winning share turns it into $1 of cash without a fill. The
-- settlement sweeper records what it removed from winning lanes so that cash
-- is counted as trading, not as a deposit.
CREATE TABLE IF NOT EXISTS settlement_payouts (
    id BIGSERIAL PRIMARY KEY,
    condition_id TEXT NOT NULL,
    token_id TEXT NOT NULL,
    unique_tag TEXT NOT NULL,
    shares NUMERIC(38, 18) NOT NULL CHECK (shares > 0),
    payout_usd NUMERIC(38, 18) NOT NULL CHECK (payout_usd >= 0),
    settled_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    equity_snapshot_id BIGINT REFERENCES equity_snapshots (id)
);

-- Each fill and payout counts toward the trade cash of exactly one snapshot:
-- the first whose valuation began after it was stored. Timestamps cannot do
-- this, since a fill may be stored long after its match time and payouts are
-- stamped by a different clock from the wallet's balance.
ALTER TABLE fills ADD COLUMN IF NOT EXISTS equity_snapshot_id BIGINT REFERENCES equity_snapshots (id);
CREATE INDEX IF NOT EXISTS fills_uncounted_idx ON fills (fill_id) WHERE equity_snapshot_id IS NULL;
CREATE INDEX IF NOT EXISTS settlement_payouts_uncounted_idx ON settlement_payouts (id) WHERE equity_snapshot_id IS NULL;
