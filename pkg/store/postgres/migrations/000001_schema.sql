-- executiond store schema: the single authoritative migration.
--
-- The execution daemon has never run against Postgres, so what was previously
-- spread across 000001 (tables), 000002 (fill-settlement columns), and
-- 000004 (target_usd rename) is folded into this one file:
--   * fills.fee_rate_bps / trade_status / trader_side and order_intents.kind
--     are declared directly here (000002 became a no-op once 000001 was edited
--     in place);
--   * order_intents.target_usd is declared nullable: it sizes BUY (open)
--     intents and is NULL for CLOSE intents, which size by the position they
--     exit (000004's legacy target_shares rename is not needed because no
--     database predates this file).

CREATE TABLE IF NOT EXISTS order_intents (
    intent_id TEXT PRIMARY KEY,
    unique_tag TEXT NOT NULL,
    strategy TEXT NOT NULL,
    kind TEXT NOT NULL,
    market_id TEXT NOT NULL DEFAULT '',
    event_slug TEXT NOT NULL DEFAULT '',
    condition_id TEXT NOT NULL,
    token_id TEXT NOT NULL,
    outcome TEXT NOT NULL,
    side TEXT NOT NULL CHECK (side IN ('BUY', 'SELL')),
    -- target_usd is set for BUY (open) intents and NULL for CLOSE intents,
    -- which size by the position they exit instead.
    target_usd NUMERIC(38, 18) CHECK (target_usd > 0),
    limit_price NUMERIC(38, 18) NOT NULL CHECK (limit_price > 0 AND limit_price < 1),
    time_in_force TEXT NOT NULL,
    post_only BOOLEAN NOT NULL DEFAULT FALSE,
    feature_seq BIGINT NOT NULL DEFAULT 0,
    feature_completed_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    status TEXT NOT NULL,
    policy JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS orders (
    intent_id TEXT NOT NULL REFERENCES order_intents(intent_id),
    child_sequence INTEGER NOT NULL CHECK (child_sequence > 0),
    signed_payload JSONB NOT NULL,
    signed_order_hash TEXT NOT NULL UNIQUE,
    salt TEXT NOT NULL,
    exchange_order_id TEXT UNIQUE,
    requested_shares NUMERIC(38, 18) NOT NULL CHECK (requested_shares > 0),
    matched_shares NUMERIC(38, 18) NOT NULL DEFAULT 0 CHECK (matched_shares >= 0),
    price NUMERIC(38, 18) NOT NULL CHECK (price > 0 AND price < 1),
    order_type TEXT NOT NULL,
    post_only BOOLEAN NOT NULL DEFAULT FALSE,
    state TEXT NOT NULL,
    revision BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (intent_id, child_sequence)
);

CREATE TABLE IF NOT EXISTS order_events (
    event_id TEXT PRIMARY KEY,
    intent_id TEXT NOT NULL,
    child_sequence INTEGER,
    exchange_order_id TEXT,
    from_state TEXT NOT NULL DEFAULT '',
    to_state TEXT NOT NULL,
    event TEXT NOT NULL,
    observation JSONB NOT NULL DEFAULT '{}'::jsonb,
    reason TEXT NOT NULL DEFAULT '',
    exchange_time TIMESTAMPTZ,
    received_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS order_events_intent_idx ON order_events(intent_id, created_at);
CREATE INDEX IF NOT EXISTS order_events_exchange_order_idx ON order_events(exchange_order_id) WHERE exchange_order_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS fills (
    fill_id TEXT PRIMARY KEY,
    exchange_order_id TEXT,
    intent_id TEXT,
    unique_tag TEXT NOT NULL,
    market_id TEXT NOT NULL DEFAULT '',
    condition_id TEXT NOT NULL,
    token_id TEXT NOT NULL,
    outcome TEXT NOT NULL,
    side TEXT NOT NULL CHECK (side IN ('BUY', 'SELL')),
    shares NUMERIC(38, 18) NOT NULL CHECK (shares > 0),
    price NUMERIC(38, 18) NOT NULL CHECK (price > 0 AND price < 1),
    fee NUMERIC(38, 18) NOT NULL DEFAULT 0 CHECK (fee >= 0),
    fee_rate_bps NUMERIC(38, 18) NOT NULL DEFAULT 0 CHECK (fee_rate_bps >= 0),
    trade_status TEXT NOT NULL DEFAULT 'CONFIRMED',
    trader_side TEXT NOT NULL DEFAULT '',
    exchange_time TIMESTAMPTZ,
    received_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS fills_position_idx ON fills(condition_id, token_id, exchange_time, received_at);
CREATE INDEX IF NOT EXISTS fills_order_idx ON fills(exchange_order_id) WHERE exchange_order_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS positions (
    condition_id TEXT NOT NULL,
    token_id TEXT NOT NULL,
    unique_tag TEXT NOT NULL,
    market_id TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL,
    position_size NUMERIC(38, 18) NOT NULL DEFAULT 0 CHECK (position_size >= 0),
    actual_shares NUMERIC(38, 18) NOT NULL DEFAULT 0 CHECK (actual_shares >= 0),
    available_size NUMERIC(38, 18) NOT NULL DEFAULT 0 CHECK (available_size >= 0),
    reserved_size NUMERIC(38, 18) NOT NULL DEFAULT 0 CHECK (reserved_size >= 0),
    entry_price NUMERIC(38, 18),
    entry_time TIMESTAMPTZ,
    state TEXT NOT NULL DEFAULT 'empty',
    source_revision BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (condition_id, token_id, unique_tag),
    CHECK (available_size + reserved_size <= position_size)
);

CREATE TABLE IF NOT EXISTS reservations (
    reservation_id TEXT PRIMARY KEY,
    intent_id TEXT NOT NULL,
    child_sequence INTEGER,
    unique_tag TEXT NOT NULL,
    market_id TEXT NOT NULL DEFAULT '',
    condition_id TEXT NOT NULL,
    token_id TEXT NOT NULL,
    outcome TEXT NOT NULL,
    side TEXT NOT NULL CHECK (side IN ('BUY', 'SELL')),
    shares NUMERIC(38, 18) NOT NULL DEFAULT 0 CHECK (shares >= 0),
    notional NUMERIC(38, 18) NOT NULL DEFAULT 0 CHECK (notional >= 0),
    state TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS reservations_position_idx ON reservations(condition_id, token_id, state);
CREATE INDEX IF NOT EXISTS reservations_intent_idx ON reservations(intent_id);
-- Canonical "one active SELL per lane" guard: positions are keyed by unique_tag,
-- so independent lanes on the same token each hold their own position row and
-- may each reserve a sell without serializing on one another.
CREATE UNIQUE INDEX IF NOT EXISTS reservations_one_active_sell_idx ON reservations(condition_id, token_id, unique_tag) WHERE side = 'SELL' AND state = 'active';
