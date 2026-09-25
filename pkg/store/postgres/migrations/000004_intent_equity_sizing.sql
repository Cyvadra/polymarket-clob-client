-- An open request may be sized as a fraction of wallet equity rather than in
-- dollars. target_usd keeps the resolved amount; these record what it was
-- resolved from, so an entry's size can be explained afterwards.
ALTER TABLE order_intents ADD COLUMN IF NOT EXISTS target_equity_fraction NUMERIC(38, 18);
ALTER TABLE order_intents ADD COLUMN IF NOT EXISTS sized_equity_usd NUMERIC(38, 18);
