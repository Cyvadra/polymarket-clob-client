package store

import (
	"testing"
	"time"
)

func TestBuildPositionFeatureOpenPosition(t *testing.T) {
	entryTime := time.Unix(100, 0).UTC()
	publishedAt := time.Unix(130, 0).UTC()
	feature := BuildPositionFeature(PositionRecord{
		MarketID:       "market",
		ConditionID:    "condition",
		TokenID:        "token",
		Outcome:        "Up",
		PositionSize:   "5.5",
		AvailableSize:  "4.5",
		ReservedSize:   "1",
		EntryPrice:     "0.42",
		EntryTime:      entryTime,
		State:          "confirmed",
		SourceRevision: 9,
		UpdatedAt:      time.Unix(120, 0).UTC(),
	}, 11, publishedAt)

	if !feature.HasPosition {
		t.Fatal("expected open position")
	}
	if feature.EntryPrice == nil || *feature.EntryPrice != "0.42" {
		t.Fatalf("entry price=%v", feature.EntryPrice)
	}
	if feature.EntryTime == nil || !feature.EntryTime.Equal(entryTime) {
		t.Fatalf("entry time=%v", feature.EntryTime)
	}
	if feature.SecondsSinceEntry != 30 {
		t.Fatalf("seconds since entry=%v", feature.SecondsSinceEntry)
	}
	if feature.PositionSize != "5.5" || feature.AvailableSize != "4.5" || feature.ReservedSize != "1" {
		t.Fatalf("unexpected sizes: %+v", feature)
	}
}

func TestBuildPositionFeatureEmptyPosition(t *testing.T) {
	feature := BuildPositionFeature(PositionRecord{
		ConditionID:   "condition",
		TokenID:       "token",
		Outcome:       "Down",
		PositionSize:  "0",
		AvailableSize: "0",
		ReservedSize:  "0",
		EntryPrice:    "0.50",
		EntryTime:     time.Unix(100, 0).UTC(),
		UpdatedAt:     time.Unix(120, 0).UTC(),
	}, 12, time.Unix(130, 0).UTC())

	if feature.HasPosition {
		t.Fatal("expected empty position")
	}
	if feature.EntryPrice != nil || feature.EntryTime != nil {
		t.Fatalf("expected nil entry fields, got %+v", feature)
	}
	if feature.SecondsSinceEntry != 0 {
		t.Fatalf("seconds since entry=%v", feature.SecondsSinceEntry)
	}
}
