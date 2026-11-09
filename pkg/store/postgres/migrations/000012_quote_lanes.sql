-- Quote lanes: a strategy keeps saying which cents it wants a post-only buy
-- resting at (strategy.execution.quote), and executiond keeps, cancels and
-- posts levels to match. One row per lane holds what must survive a restart:
-- the last applied seq, the deadline, whether the first fill ended the lane,
-- and whether its one result is still owed. The levels themselves are child
-- orders of the lane's one QUOTE intent (intent_id).
CREATE TABLE IF NOT EXISTS quote_lanes (
    unique_tag TEXT NOT NULL,
    condition_id TEXT NOT NULL,
    token_id TEXT NOT NULL,
    intent_id TEXT NOT NULL DEFAULT '',
    last_seq BIGINT NOT NULL DEFAULT 0,
    deadline TIMESTAMPTZ,
    done BOOLEAN NOT NULL DEFAULT FALSE,
    -- '' (none), OWED (the first fill is known, its result not yet sent) or
    -- PUBLISHED.
    result_state TEXT NOT NULL DEFAULT '',
    filled_sequence INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (unique_tag, condition_id, token_id)
);

CREATE INDEX IF NOT EXISTS quote_lanes_intent_idx ON quote_lanes (intent_id) WHERE intent_id <> '';
CREATE INDEX IF NOT EXISTS quote_lanes_owed_idx ON quote_lanes (unique_tag) WHERE result_state = 'OWED';

-- The request that posted each level, echoed on the result when it fills.
CREATE TABLE IF NOT EXISTS quote_levels (
    intent_id TEXT NOT NULL,
    child_sequence INTEGER NOT NULL,
    request_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (intent_id, child_sequence)
);
