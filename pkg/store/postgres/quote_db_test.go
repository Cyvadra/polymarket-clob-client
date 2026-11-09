//go:build postgres

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

func quoteLaneKey(t *testing.T, s *Store, lane testLane) store.QuoteLaneKey {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.pool.Exec(ctx, `DELETE FROM quote_levels WHERE intent_id LIKE $1`, lane.tag+"%")
		_, _ = s.pool.Exec(ctx, `DELETE FROM quote_lanes WHERE unique_tag = $1`, lane.tag)
	})
	return store.QuoteLaneKey{UniqueTag: lane.tag, ConditionID: lane.conditionID, TokenID: lane.tokenID}
}

func TestQuoteLaneSaveNeverLowersSeqAndDoneIsFinal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := quoteLaneKey(t, s, newTestLane(t, s))
	if _, found, err := s.QuoteLane(ctx, key); err != nil || found {
		t.Fatalf("new lane: found=%v err=%v", found, err)
	}
	deadline := time.Now().Add(3 * time.Second).UTC().Truncate(time.Microsecond)
	if err := s.SaveQuoteLane(ctx, store.QuoteLaneRecord{Key: key, IntentID: key.UniqueTag + "-intent", LastSeq: 10, Deadline: deadline}); err != nil {
		t.Fatal(err)
	}
	// A lower seq and an equal one change nothing.
	for _, seq := range []int64{9, 10} {
		if err := s.SaveQuoteLane(ctx, store.QuoteLaneRecord{Key: key, LastSeq: seq, Deadline: deadline.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	lane, found, err := s.QuoteLane(ctx, key)
	if err != nil || !found || lane.LastSeq != 10 || !lane.Deadline.Equal(deadline) || lane.IntentID != key.UniqueTag+"-intent" {
		t.Fatalf("lane=%+v found=%v err=%v", lane, found, err)
	}
	byIntent, found, err := s.QuoteLaneByIntent(ctx, key.UniqueTag+"-intent")
	if err != nil || !found || byIntent.Key != key {
		t.Fatalf("by intent: %+v found=%v err=%v", byIntent, found, err)
	}

	marked, err := s.MarkQuoteDone(ctx, key, "", 3)
	if err != nil || !marked {
		t.Fatalf("mark done: marked=%v err=%v", marked, err)
	}
	if marked, err := s.MarkQuoteDone(ctx, key, "", 4); err != nil || marked {
		t.Fatalf("second mark done: marked=%v err=%v", marked, err)
	}
	if err := s.SaveQuoteLane(ctx, store.QuoteLaneRecord{Key: key, LastSeq: 20}); err != nil {
		t.Fatal(err)
	}
	lane, _, _ = s.QuoteLane(ctx, key)
	if !lane.Done || lane.LastSeq != 10 || lane.FilledSequence != 3 || lane.ResultState != store.QuoteResultOwed || lane.IntentID == "" {
		t.Fatalf("done lane=%+v", lane)
	}
	owed, err := s.OwedQuoteResults(ctx)
	if err != nil || !containsLane(owed, key) {
		t.Fatalf("owed=%+v err=%v", owed, err)
	}
	if moved, err := s.MoveQuoteResult(ctx, key, store.QuoteResultPublished, store.QuoteResultOwed); err != nil || !moved {
		t.Fatalf("claim: moved=%v err=%v", moved, err)
	}
	if moved, err := s.MoveQuoteResult(ctx, key, store.QuoteResultPublished, store.QuoteResultOwed); err != nil || moved {
		t.Fatalf("second claim: moved=%v err=%v", moved, err)
	}
	if owed, _ := s.OwedQuoteResults(ctx); containsLane(owed, key) {
		t.Fatal("a published result is still owed")
	}
}

func TestQuoteLaneMarkDoneCreatesAMissingLane(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := quoteLaneKey(t, s, newTestLane(t, s))
	if marked, err := s.MarkQuoteDone(ctx, key, key.UniqueTag+"-intent", 1); err != nil || !marked {
		t.Fatalf("marked=%v err=%v", marked, err)
	}
	lane, found, _ := s.QuoteLane(ctx, key)
	if !found || !lane.Done {
		t.Fatalf("lane=%+v found=%v", lane, found)
	}
}

func TestQuoteLevelRequestRoundTrips(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	lane := newTestLane(t, s)
	quoteLaneKey(t, s, lane)
	intentID := lane.tag + "-intent"
	if got, err := s.QuoteLevelRequest(ctx, intentID, 1); err != nil || got != "" {
		t.Fatalf("unrecorded level: %q %v", got, err)
	}
	if err := s.RecordQuoteLevel(ctx, intentID, 1, "req-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.QuoteLevelRequest(ctx, intentID, 1); err != nil || got != "req-1" {
		t.Fatalf("level request: %q %v", got, err)
	}
}

func containsLane(lanes []store.QuoteLaneRecord, key store.QuoteLaneKey) bool {
	for _, lane := range lanes {
		if lane.Key == key {
			return true
		}
	}
	return false
}
