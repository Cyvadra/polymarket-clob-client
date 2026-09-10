-- Positions used to be credited from fills with a modelled 7% taker-buy fee
-- taken in shares. The exchange takes no such fee in shares, so those rows
-- under-count what the wallet holds, and a close sized from them leaves real
-- shares behind. Rebuild each lane's size from the net of its recorded fills,
-- the rule the fill path now applies. Only rows whose size disagrees change.
WITH fill_totals AS (
    SELECT condition_id, token_id, unique_tag,
        GREATEST(SUM(CASE WHEN side = 'BUY' THEN shares ELSE -shares END), 0) AS net,
        SUM(CASE WHEN side = 'BUY' THEN shares * price END)
            / NULLIF(SUM(CASE WHEN side = 'BUY' THEN shares END), 0) AS buy_price,
        MIN(CASE WHEN side = 'BUY' THEN received_at END) AS first_buy
    FROM fills
    WHERE trade_status <> 'FAILED'
    GROUP BY condition_id, token_id, unique_tag
),
corrected AS (
    SELECT p.condition_id, p.token_id, p.unique_tag, t.net,
        LEAST(p.reserved_size, t.net) AS reserved,
        t.buy_price, t.first_buy
    FROM positions p
    JOIN fill_totals t USING (condition_id, token_id, unique_tag)
    WHERE p.position_size <> t.net OR p.actual_shares <> t.net
)
UPDATE positions p SET
    position_size = c.net,
    actual_shares = c.net,
    reserved_size = c.reserved,
    available_size = c.net - c.reserved,
    entry_price = CASE WHEN c.net > 0 THEN COALESCE(p.entry_price, c.buy_price) END,
    entry_time = CASE WHEN c.net > 0 THEN COALESCE(p.entry_time, c.first_buy) END,
    state = CASE WHEN c.net = 0 THEN 'empty' WHEN p.state = 'unsettled' THEN 'unsettled' ELSE 'open' END,
    source_revision = p.source_revision + 1,
    updated_at = now()
FROM corrected c
WHERE p.condition_id = c.condition_id AND p.token_id = c.token_id AND p.unique_tag = c.unique_tag;
