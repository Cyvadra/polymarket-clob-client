package accountfeed

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

func TestPendingReplaysHeldInArrivalOrder(t *testing.T) {
	p := NewPendingOrders(context.Background(), nil)
	p.Expect("o1")
	var got []int
	for i := range 3 {
		if !p.Hold("o1", func(context.Context) { got = append(got, i) }) {
			t.Fatal("expected order not held")
		}
	}
	p.Stored("o1")
	if !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Fatalf("replayed %v, want 0 1 2", got)
	}
	if p.Hold("o1", func(context.Context) {}) {
		t.Fatal("stored order still held")
	}
}

func TestPendingQueuesEventsArrivingDuringReplay(t *testing.T) {
	p := NewPendingOrders(context.Background(), nil)
	p.Expect("o1")
	var got []string
	p.Hold("o1", func(context.Context) {
		got = append(got, "first")
		// An event the stream delivers while the replay runs waits behind it.
		if !p.Hold("o1", func(context.Context) { got = append(got, "late") }) {
			t.Error("event during replay was not held")
		}
	})
	p.Stored("o1")
	if !reflect.DeepEqual(got, []string{"first", "late"}) {
		t.Fatalf("replayed %v", got)
	}
}

func TestPendingForgetDropsHeld(t *testing.T) {
	p := NewPendingOrders(context.Background(), nil)
	p.Expect("o1")
	ran := false
	p.Hold("o1", func(context.Context) { ran = true })
	p.Forget("o1")
	p.Stored("o1")
	if ran {
		t.Fatal("forgotten event replayed")
	}
}

func TestPendingIgnoresUnexpectedAndNil(t *testing.T) {
	p := NewPendingOrders(context.Background(), nil)
	if p.Hold("other", func(context.Context) {}) {
		t.Fatal("unexpected order held")
	}
	var none *PendingOrders
	none.Expect("o1")
	if none.Hold("o1", func(context.Context) {}) {
		t.Fatal("nil registry held an event")
	}
	none.Stored("o1")
	none.Forget("o1")
}

func TestPendingExpiresUnresolvedOrder(t *testing.T) {
	var mu sync.Mutex
	var reports []error
	p := NewPendingOrders(context.Background(), func(err error) { mu.Lock(); reports = append(reports, err); mu.Unlock() })
	p.timeout = 20 * time.Millisecond
	p.Expect("o1")
	p.Hold("o1", func(context.Context) { t.Error("expired event replayed") })
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 1 || !strings.Contains(reports[0].Error(), "dropped 1") {
		t.Fatalf("reports = %v", reports)
	}
	if p.Hold("o1", func(context.Context) {}) {
		t.Fatal("expired order still held")
	}
}

func TestOrderEventBeforeRowIsAppliedOnceStored(t *testing.T) {
	repository := &fakeOrderStore{}
	consumer, _ := NewOrderConsumer(repository, time.Now)
	pending := NewPendingOrders(context.Background(), nil)
	consumer.SetPendingOrders(pending)
	pending.Expect("order-1")
	event := AccountOrderEvent{SchemaVersion: protocol.SchemaVersionV1, EventID: "e1", ExchangeOrderID: "order-1", Status: "CANCELED", MatchedShares: "0"}
	if err := consumer.Consume(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if repository.updated != "" {
		t.Fatal("event applied before the row existed")
	}
	repository.order = store.SignedOrderRecord{IntentID: "intent-1", ChildSequence: 1, ExchangeOrderID: "order-1", State: statemachine.StateLive, Revision: 2}
	pending.Stored("order-1")
	if repository.updated != statemachine.StateCanceled {
		t.Fatalf("held event not applied: state %q", repository.updated)
	}
}

// lateFillStore has no row for order-late until stored is set.
type lateFillStore struct {
	*fakeFillStore
	stored bool
}

func (s *lateFillStore) OrderByExchangeID(ctx context.Context, orderID string) (store.SignedOrderRecord, error) {
	if orderID == "order-late" && s.stored {
		return s.fakeFillStore.OrderByExchangeID(ctx, "order-1")
	}
	return store.SignedOrderRecord{}, store.ErrNotFound
}

func TestFillBeforeRowIsAppliedOnceStoredAndNotReported(t *testing.T) {
	repository := &lateFillStore{fakeFillStore: &fakeFillStore{}}
	consumer, _ := NewFillConsumer(repository, time.Now)
	var reports []error
	consumer.SetErrorHandler(func(err error) { reports = append(reports, err) })
	pending := NewPendingOrders(context.Background(), nil)
	consumer.SetPendingOrders(pending)
	pending.Expect("order-late")
	fill := testFill()
	fill.ExchangeOrderID = "order-late"
	if inserted, err := consumer.Consume(context.Background(), fill); err != nil || inserted {
		t.Fatalf("held fill: inserted=%v err=%v", inserted, err)
	}
	repository.stored = true
	pending.Stored("order-late")
	if repository.applied != 1 {
		t.Fatalf("held fill applied %d times, want 1", repository.applied)
	}
	if len(reports) != 0 {
		t.Fatalf("held fill reported: %v", reports)
	}
}

func TestReplayStillUnknownIsReportedNotRequeued(t *testing.T) {
	repository := &lateFillStore{fakeFillStore: &fakeFillStore{}}
	consumer, _ := NewFillConsumer(repository, time.Now)
	var reports []error
	consumer.SetErrorHandler(func(err error) { reports = append(reports, err) })
	pending := NewPendingOrders(context.Background(), nil)
	consumer.SetPendingOrders(pending)
	pending.Expect("order-late")
	fill := testFill()
	fill.ExchangeOrderID = "order-late"
	_, _ = consumer.Consume(context.Background(), fill)
	pending.Stored("order-late") // row still missing: must not loop
	if len(reports) != 1 || errors.Is(reports[0], context.Canceled) {
		t.Fatalf("reports = %v, want one unknown-fill report", reports)
	}
}
