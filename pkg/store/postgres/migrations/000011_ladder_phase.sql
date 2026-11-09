-- ladder_phase was added to 000010 after that file had already been applied
-- (2026-11-07) on running databases, and an applied migration is never run
-- again. Everything here is idempotent, so a database that got the column from
-- the edited 000010 passes through unchanged.
ALTER TABLE order_intents
    ADD COLUMN IF NOT EXISTS ladder_phase TEXT NOT NULL DEFAULT '';

-- The recovery pass looks for ladders whose result is still owed every second.
CREATE INDEX IF NOT EXISTS order_intents_ladder_pending_idx ON order_intents (intent_id)
    WHERE ladder_phase IN ('ACTIVE', 'STOPPING');
