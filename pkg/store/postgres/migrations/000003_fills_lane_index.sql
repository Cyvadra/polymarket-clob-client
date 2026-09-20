-- Every position feature frame now counts the lane's open lots from its fills
-- (see positionSelectSQL), and that runs for every open position on the
-- publisher's tick. fills_position_idx starts at (condition_id, token_id),
-- which leaves the lane and side to a filter; this index narrows the scan to
-- the lane's buys since entry.
CREATE INDEX IF NOT EXISTS fills_lane_idx ON fills(unique_tag, condition_id, token_id, side, received_at);
