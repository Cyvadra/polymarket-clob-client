-- When an operation was armed, its baseline read: an operation resolved
-- from the wallet's balances discounts the exchange fills since then, which
-- move the same balances.
ALTER TABLE ctf_operations ADD COLUMN IF NOT EXISTS armed_at TIMESTAMPTZ;
