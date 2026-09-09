package accountfeed

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeFillStore struct {
	record  store.FillRecord
	applied int
	seen    map[string]struct{}
}

func (s *fakeFillStore) OrderByExchangeID(_ context.Context, orderID string) (store.SignedOrderRecord, error) {
	if orderID != "order-1" {
		return store.SignedOrderRecord{}, store.ErrNotFound
	}
	return store.SignedOrderRecord{IntentID: "intent-1", ExchangeOrderID: orderID}, nil
}

func (s *fakeFillStore) Intent(_ context.Context, intentID string) (store.OrderIntentRecord, error) {
	if intentID != "intent-1" {
		return store.OrderIntentRecord{}, store.ErrNotFound
	}
	return store.OrderIntentRecord{IntentID: intentID, UniqueTag: "lane-a"}, nil
}

func (s *fakeFillStore) ApplyFill(_ context.Context, record store.FillRecord) (bool, error) {
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	if _, exists := s.seen[record.FillID]; exists {
		return false, nil
	}
	s.seen[record.FillID] = struct{}{}
	s.record = record
	s.applied++
	return true, nil
}

func TestConsumeMapsValidatedFillToStore(t *testing.T) {
	now := time.Unix(20, 0).UTC()
	repository := &fakeFillStore{}
	consumer, err := NewFillConsumer(repository, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new fill consumer: %v", err)
	}
	inserted, err := consumer.Consume(context.Background(), testFill())
	if err != nil {
		t.Fatalf("consume fill: %v", err)
	}
	if !inserted || repository.record.FillID != "fill-1" || repository.record.UniqueTag != "lane-a" || repository.record.ReceivedAt != now {
		t.Fatalf("unexpected persisted fill: inserted=%v record=%+v", inserted, repository.record)
	}
}

func TestConsumeRejectsInvalidDecimalBeforeStore(t *testing.T) {
	repository := &fakeFillStore{}
	consumer, err := NewFillConsumer(repository, time.Now)
	if err != nil {
		t.Fatalf("new fill consumer: %v", err)
	}
	fill := testFill()
	fill.Shares = "0"
	if _, err := consumer.Consume(context.Background(), fill); err == nil {
		t.Fatal("expected invalid shares to be rejected")
	}
	if repository.record.FillID != "" {
		t.Fatalf("store received invalid fill: %+v", repository.record)
	}
}

func testFill() AccountFill {
	return AccountFill{
		SchemaVersion:   protocol.SchemaVersionV1,
		FillID:          "fill-1",
		ExchangeOrderID: "order-1",
		ConditionID:     "condition",
		TokenID:         "token",
		Outcome:         "Up",
		Side:            protocol.SideBuy,
		Shares:          "2.5",
		Price:           "0.42",
	}
}

func TestUnknownOrderReportsAreDedupedAndCapped(t *testing.T) {
	repository := &fakeFillStore{}
	consumer, err := NewFillConsumer(repository, time.Now)
	if err != nil {
		t.Fatalf("new fill consumer: %v", err)
	}
	var reports []string
	consumer.SetErrorHandler(func(err error) { reports = append(reports, err.Error()) })

	// The same order filling repeatedly, and the reconciler replaying it on
	// every pass, must not report more than once.
	for i := 0; i < 5; i++ {
		fill := testFill()
		fill.FillID = fmt.Sprintf("fill-%d", i)
		fill.ExchangeOrderID = "outside-order"
		if inserted, err := consumer.Consume(context.Background(), fill); err != nil || inserted {
			t.Fatalf("consume unknown fill: inserted=%v err=%v", inserted, err)
		}
	}
	if len(reports) != 1 {
		t.Fatalf("repeated fills for one order reported %d times: %v", len(reports), reports)
	}

	for i := 1; i < 40; i++ {
		fill := testFill()
		fill.FillID = fmt.Sprintf("other-fill-%d", i)
		fill.ExchangeOrderID = fmt.Sprintf("other-order-%d", i)
		if _, err := consumer.Consume(context.Background(), fill); err != nil {
			t.Fatalf("consume unknown fill: %v", err)
		}
	}
	if len(reports) != unknownOrderReportLimit {
		t.Fatalf("reported %d lines, want the %d line cap: %v", len(reports), unknownOrderReportLimit, reports)
	}
	if !strings.Contains(reports[len(reports)-1], "suppressing further per-order reports") {
		t.Fatalf("last report is not the suppression summary: %q", reports[len(reports)-1])
	}
	if consumer.UnknownOrderCount() != 40 {
		t.Fatalf("unknown order count = %d, want 40", consumer.UnknownOrderCount())
	}
}
