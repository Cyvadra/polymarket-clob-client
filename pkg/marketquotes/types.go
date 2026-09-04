package marketquotes

import "time"

type Quote struct {
	AssetID   string    `json:"asset_id"`
	Bid       float64   `json:"bid"`
	Ask       float64   `json:"ask"`
	Mid       float64   `json:"mid"`
	Timestamp time.Time `json:"timestamp"`
}

// Snapshot is consumed by execution tactics when planning quote-aware child orders.
type Snapshot struct {
	ConditionID string    `json:"condition_id"`
	At          time.Time `json:"at"`
	Up          Quote     `json:"up"`
	Down        Quote     `json:"down"`
}
