package store

import (
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/contracts"
)

func BuildPositionFeature(position PositionRecord, sequence int64, publishedAt time.Time) contracts.PositionFeature {
	var entryPrice *string
	if position.EntryPrice != "" && position.PositionSize != "" && position.PositionSize != "0" {
		entryPrice = &position.EntryPrice
	}

	var entryTime *time.Time
	secondsSinceEntry := 0.0
	if entryPrice != nil && !position.EntryTime.IsZero() {
		entry := position.EntryTime.UTC()
		entryTime = &entry
		if publishedAt.After(entry) {
			secondsSinceEntry = publishedAt.Sub(entry).Seconds()
		}
	}

	return contracts.PositionFeature{
		SchemaVersion:     contracts.SchemaVersionV1,
		Seq:               sequence,
		MarketID:          position.MarketID,
		ConditionID:       position.ConditionID,
		TokenID:           position.TokenID,
		Outcome:           position.Outcome,
		HasPosition:       entryPrice != nil,
		EntryPrice:        entryPrice,
		EntryTime:         entryTime,
		SecondsSinceEntry: secondsSinceEntry,
		PositionSize:      position.PositionSize,
		AvailableSize:     position.AvailableSize,
		ReservedSize:      position.ReservedSize,
		State:             position.State,
		SourceRevision:    position.SourceRevision,
		UpdatedAt:         position.UpdatedAt.UTC(),
		PublishedAt:       publishedAt.UTC(),
	}
}
