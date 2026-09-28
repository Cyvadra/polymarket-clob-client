-- A lane emptied as a losing token is recorded too, with no payout, so the
-- snapshot that counts it knows a loss settled in its interval. Kept only in
-- memory, a loss swept before a restart and not yet snapshotted was dropped.
ALTER TABLE settlement_payouts ADD COLUMN IF NOT EXISTS lost BOOLEAN NOT NULL DEFAULT false;
