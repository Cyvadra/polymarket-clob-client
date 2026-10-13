-- Each equity snapshot also splits the wallet's trading by strategy, so a
-- drawdown limit can be set per strategy. A lane belongs to the strategy of
-- the intents that trade it. trade_index chains the strategy's share of each
-- interval's trading return, the profit its lanes made over the wallet equity
-- it was earned on, so the strategies' returns add up to the wallet's.
CREATE TABLE IF NOT EXISTS equity_snapshot_strategies (
    snapshot_id BIGINT NOT NULL REFERENCES equity_snapshots (id),
    strategy TEXT NOT NULL,
    trade_cash_usd NUMERIC(38, 18) NOT NULL,
    positions_usd NUMERIC(38, 18) NOT NULL,
    trade_index NUMERIC(38, 18) NOT NULL,
    PRIMARY KEY (snapshot_id, strategy)
);
CREATE INDEX IF NOT EXISTS equity_snapshot_strategies_strategy_idx ON equity_snapshot_strategies (strategy, snapshot_id);
-- Plain, not CONCURRENTLY: Migrate runs every migration in one transaction.
CREATE INDEX IF NOT EXISTS order_intents_lane_idx ON order_intents (condition_id, token_id, unique_tag, created_at, intent_id)
    WHERE kind = 'OPEN' AND strategy <> '';
