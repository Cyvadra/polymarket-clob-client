-- The requester's id for the request an intent came from, echoed on the
-- intent's results so a strategy engine talking to several executiond
-- instances can match each result to the request it answers. Empty when the
-- request carried none.
ALTER TABLE order_intents ADD COLUMN IF NOT EXISTS request_id TEXT NOT NULL DEFAULT '';
