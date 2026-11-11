-- Operational history that used to live only in executiond's logs, so a
-- maintainer answers "what happened" with a query.

-- One row per executiond process: who it signed as and what it was told.
CREATE TABLE IF NOT EXISTS daemon_runs (
    run_id BIGSERIAL PRIMARY KEY,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    stopped_at TIMESTAMPTZ,
    version TEXT NOT NULL DEFAULT '',
    signer TEXT NOT NULL,
    maker TEXT NOT NULL,
    strategies TEXT[] NOT NULL DEFAULT '{}',
    -- Non-secret settings: multipliers, limits, intervals.
    config JSONB NOT NULL DEFAULT '{}'::jsonb
);

-- One row per settlement sweep pass that found anything or failed.
CREATE TABLE IF NOT EXISTS settlement_sweeps (
    sweep_id BIGSERIAL PRIMARY KEY,
    run_id BIGINT REFERENCES daemon_runs(run_id),
    swept_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    checked INTEGER NOT NULL,
    lost INTEGER NOT NULL,
    redeemed INTEGER NOT NULL,
    dust INTEGER NOT NULL,
    shrunk INTEGER NOT NULL,
    closing INTEGER NOT NULL,
    settling INTEGER NOT NULL,
    failed BOOLEAN NOT NULL
);

CREATE INDEX IF NOT EXISTS settlement_sweeps_swept_at_idx ON settlement_sweeps (swept_at);

-- Risk and sizing decisions: drawdown suspend and resume, sizing-base
-- changes, deferred equity snapshots.
CREATE TABLE IF NOT EXISTS risk_events (
    event_id BIGSERIAL PRIMARY KEY,
    run_id BIGINT REFERENCES daemon_runs(run_id),
    at TIMESTAMPTZ NOT NULL DEFAULT now(),
    kind TEXT NOT NULL,
    -- The guard or strategy the event is about, '' for the wallet.
    subject TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    details JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS risk_events_at_idx ON risk_events (at);

-- Intents per day by strategy, kind and status.
CREATE OR REPLACE VIEW v_intents_daily AS
SELECT date_trunc('day', created_at) AS day, strategy, kind, status, count(*) AS intents
FROM order_intents
GROUP BY 1, 2, 3, 4;
