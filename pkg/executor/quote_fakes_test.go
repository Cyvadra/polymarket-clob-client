package executor

import (
	"context"
	"sync"

	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// quoteMem is an in-memory store.QuoteStore, embedded in fakeStore.
type quoteMem struct {
	quoteMu     sync.Mutex
	quoteLanes  map[store.QuoteLaneKey]store.QuoteLaneRecord
	quoteLevels map[orderKey]string
	// markDoneCalls counts MarkQuoteDone calls that ended a lane.
	markDoneCalls int
	// failSave fails every SaveQuoteLane.
	failSave error
}

func (q *quoteMem) laneMap() map[store.QuoteLaneKey]store.QuoteLaneRecord {
	if q.quoteLanes == nil {
		q.quoteLanes = map[store.QuoteLaneKey]store.QuoteLaneRecord{}
		q.quoteLevels = map[orderKey]string{}
	}
	return q.quoteLanes
}

func (q *quoteMem) QuoteLane(_ context.Context, key store.QuoteLaneKey) (store.QuoteLaneRecord, bool, error) {
	q.quoteMu.Lock()
	defer q.quoteMu.Unlock()
	lane, ok := q.laneMap()[key]
	return lane, ok, nil
}

func (q *quoteMem) QuoteLaneByIntent(_ context.Context, intentID string) (store.QuoteLaneRecord, bool, error) {
	q.quoteMu.Lock()
	defer q.quoteMu.Unlock()
	for _, lane := range q.laneMap() {
		if lane.IntentID == intentID && intentID != "" {
			return lane, true, nil
		}
	}
	return store.QuoteLaneRecord{}, false, nil
}

func (q *quoteMem) SaveQuoteLane(_ context.Context, record store.QuoteLaneRecord) error {
	q.quoteMu.Lock()
	defer q.quoteMu.Unlock()
	if q.failSave != nil {
		return q.failSave
	}
	lanes := q.laneMap()
	lane, ok := lanes[record.Key]
	if ok && (lane.Done || lane.LastSeq >= record.LastSeq) {
		return nil
	}
	lane.Key = record.Key
	if record.IntentID != "" {
		lane.IntentID = record.IntentID
	}
	lane.LastSeq, lane.Deadline = record.LastSeq, record.Deadline
	lanes[record.Key] = lane
	return nil
}

func (q *quoteMem) MarkQuoteDone(_ context.Context, key store.QuoteLaneKey, intentID string, sequence int) (bool, error) {
	q.quoteMu.Lock()
	defer q.quoteMu.Unlock()
	lanes := q.laneMap()
	lane := lanes[key]
	if lane.Done {
		return false, nil
	}
	lane.Key, lane.Done, lane.ResultState, lane.FilledSequence = key, true, store.QuoteResultOwed, sequence
	if intentID != "" {
		lane.IntentID = intentID
	}
	lanes[key] = lane
	q.markDoneCalls++
	return true, nil
}

func (q *quoteMem) MoveQuoteResult(_ context.Context, key store.QuoteLaneKey, to, from store.QuoteResultState) (bool, error) {
	q.quoteMu.Lock()
	defer q.quoteMu.Unlock()
	lanes := q.laneMap()
	lane, ok := lanes[key]
	if !ok || lane.ResultState != from {
		return false, nil
	}
	lane.ResultState = to
	lanes[key] = lane
	return true, nil
}

func (q *quoteMem) OwedQuoteResults(context.Context) ([]store.QuoteLaneRecord, error) {
	q.quoteMu.Lock()
	defer q.quoteMu.Unlock()
	var owed []store.QuoteLaneRecord
	for _, lane := range q.laneMap() {
		if lane.ResultState == store.QuoteResultOwed {
			owed = append(owed, lane)
		}
	}
	return owed, nil
}

func (q *quoteMem) RecordQuoteLevel(_ context.Context, intentID string, sequence int, requestID string) error {
	q.quoteMu.Lock()
	defer q.quoteMu.Unlock()
	q.laneMap()
	q.quoteLevels[orderKey{intentID, sequence}] = requestID
	return nil
}

func (q *quoteMem) QuoteLevelRequest(_ context.Context, intentID string, sequence int) (string, error) {
	q.quoteMu.Lock()
	defer q.quoteMu.Unlock()
	q.laneMap()
	return q.quoteLevels[orderKey{intentID, sequence}], nil
}
