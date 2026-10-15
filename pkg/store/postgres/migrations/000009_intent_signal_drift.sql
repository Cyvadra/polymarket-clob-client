-- An open's drift bound: the mid it was decided on and how far the market may
-- move against it before executiond cancels it unfilled. NULL when the
-- strategy set none.
ALTER TABLE order_intents ADD COLUMN IF NOT EXISTS signal_price NUMERIC(38, 18);
ALTER TABLE order_intents ADD COLUMN IF NOT EXISTS max_signal_drift NUMERIC(38, 18);
