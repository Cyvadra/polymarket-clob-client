CREATE TABLE IF NOT EXISTS order_intents (
    intent_id TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    strategy TEXT NOT NULL,
	kind TEXT NOT NULL,
    market_id TEXT NOT NULL DEFAULT '',
    event_slug TEXT NOT NULL DEFAULT '',
    condition_id TEXT NOT NULL,
    token_id TEXT NOT NULL,
    outcome TEXT NOT NULL,
    side TEXT NOT NULL CHECK (side IN ('BUY', 'SELL')),
    target_usd NUMERIC(38, 18) NOT NULL CHECK (target_usd > 0),
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
    PRIMARY KEY (condition_id, token_id),
    CHECK (available_size + reserved_size <= position_size)
);

CREATE TABLE IF NOT EXISTS reservations (
    reservation_id TEXT PRIMARY KEY,
    intent_id TEXT NOT NULL,
    child_sequence INTEGER,
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
CREATE UNIQUE INDEX IF NOT EXISTS reservations_one_active_sell_idx ON reservations(condition_id, token_id) WHERE side = 'SELL' AND state = 'active';
