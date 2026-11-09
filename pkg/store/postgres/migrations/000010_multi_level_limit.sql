-- Multi-level limit buys: one intent posts a post-only child at every cent of
-- bottom_price..limit_price. ladder_phase is separate from status because the
-- status follows each child order's state and would overwrite a lifecycle
-- marker the next time any child moved.
ALTER TABLE order_intents
    ADD COLUMN IF NOT EXISTS bottom_price NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS scaling_mode TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS ladder_phase TEXT NOT NULL DEFAULT '';

ALTER TABLE order_intents DROP CONSTRAINT IF EXISTS order_intents_multi_level_range;
ALTER TABLE order_intents
    ADD CONSTRAINT order_intents_multi_level_range CHECK (
        bottom_price IS NULL OR
        (bottom_price > 0 AND bottom_price <= limit_price AND limit_price < 1)
    );

-- The recovery pass looks for ladders whose result is still owed every second.
CREATE INDEX IF NOT EXISTS order_intents_ladder_pending_idx ON order_intents (intent_id)
    WHERE ladder_phase IN ('ACTIVE', 'STOPPING');
