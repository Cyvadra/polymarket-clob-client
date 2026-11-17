-- Split inventory for SPLIT_PRESERVE opens. A split or merge moves tokens
-- and cash with no CLOB trade, so it is booked as fills tagged with where they
-- came from; positions, trade cash and equity then account for it unchanged.

-- CLOB: a trade on the exchange. SPLIT / MERGE: one leg of a split or merge,
-- priced at 0.5 each so a pair costs and returns exactly 1. TRANSFER: a
-- preserved share moving from the inventory lane to the strategy lane, as a
-- SELL on one and a BUY on the other at the same price, so it moves no cash.
ALTER TABLE fills ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'CLOB'
    CHECK (source IN ('CLOB', 'SPLIT', 'MERGE', 'TRANSFER'));

-- One row per split or merge sent through the relayer. unique_tag is the
-- strategy lane; its inventory lives on the lane unique_tag || '|inv'.
CREATE TABLE IF NOT EXISTS ctf_operations (
    op_id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL DEFAULT '',
    strategy TEXT NOT NULL,
    unique_tag TEXT NOT NULL,
    market_id TEXT NOT NULL DEFAULT '',
    condition_id TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('SPLIT', 'MERGE')),
    -- target_shares is what a SPLIT asked the inventory to reach; shares is
    -- the pairs this operation splits or merges. A MERGE with nothing free
    -- is recorded with 0 shares, as CONFIRMED, so the lane still closes.
    target_shares NUMERIC(38, 18),
    shares NUMERIC(38, 18) NOT NULL CHECK (shares > 0 OR (action = 'MERGE' AND shares = 0)),
    -- Each outcome token of the condition, so a recovered operation books
    -- its fills without reading the market again.
    tokens TEXT[] NOT NULL,
    outcomes TEXT[] NOT NULL,
    -- PENDING: recorded, not yet accepted by the relayer. SUBMITTED: accepted,
    -- not yet confirmed. CONFIRMED: on chain and booked. FAILED: never landed.
    -- UNKNOWN: the submission's outcome is unknown; resolved from the chain.
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'SUBMITTED', 'CONFIRMED', 'FAILED', 'UNKNOWN')),
    relayer_tx_id TEXT NOT NULL DEFAULT '',
    tx_hash TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    -- The wallet's balance of each token, in base units, read just before
    -- submission, and the batch's signed deadline: an operation whose outcome
    -- is unknown is resolved by whether both balances moved by shares, and is
    -- failed once the deadline has passed without them moving.
    baseline TEXT[] NOT NULL DEFAULT '{}',
    deadline TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS ctf_operations_lane_idx ON ctf_operations (unique_tag, condition_id, state);
CREATE INDEX IF NOT EXISTS ctf_operations_open_idx ON ctf_operations (state) WHERE state IN ('PENDING', 'SUBMITTED', 'UNKNOWN');

-- Several SPLIT_PRESERVE entries may rest against one inventory lane at once:
-- the inventory lock, not this index, keeps them within the inventory. The
-- one-active-SELL guard stays on every other lane.
DROP INDEX IF EXISTS reservations_one_active_sell_idx;
CREATE UNIQUE INDEX IF NOT EXISTS reservations_one_active_sell_idx ON reservations(condition_id, token_id, unique_tag)
    WHERE side = 'SELL' AND state = 'active' AND unique_tag NOT LIKE '%|inv';
