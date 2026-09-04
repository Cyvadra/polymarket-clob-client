package marketquotes

import "time"

type Quote struct {
	AssetID   string    `json:"asset_id"`
	Bid       float64   `json:"bid"`
	Ask       float64   `json:"ask"`
	Mid       float64   `json:"mid"`
	Timestamp time.Time `json:"timestamp"`
}

// Snapshot is held for the future execution tactics worker; executiond does
// not subscribe to or act on snapshots until that worker exists.
type Snapshot struct {
	ConditionID string    `json:"condition_id"`
	At          time.Time `json:"at"`
	Up          Quote     `json:"up"`
	Down        Quote     `json:"down"`
}
