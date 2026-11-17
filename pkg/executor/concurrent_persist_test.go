package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/accountfeed"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// gatedStore holds PersistSubmittingOrder until release is closed and can
// script its errors; it records reservation releases.
type gatedStore struct {
	*fakeStore
	release     chan struct{}
	mu          sync.Mutex
	persistErrs []error
	persists    int
	released    []string
}

func (s *gatedStore) PersistSubmittingOrder(ctx context.Context, record store.SignedOrderRecord, reason string) (store.SignedOrderRecord, error) {
	if s.release != nil {
		<-s.release
	}
	s.mu.Lock()
	s.persists++
	var err error
	if len(s.persistErrs) > 0 {
		err, s.persistErrs = s.persistErrs[0], s.persistErrs[1:]
	}
	s.mu.Unlock()
	if err != nil {
		return store.SignedOrderRecord{}, err
	}
	return s.fakeStore.PersistSubmittingOrder(ctx, record, reason)
}

func (s *gatedStore) Release(_ context.Context, reservationID, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, reservationID)
	return nil
}

// postingCLOB signals each POST and answers cancels as accepted or not.
type postingCLOB struct {
	*fakeCLOB
	posted   chan struct{}
	mu       sync.Mutex
	accept   bool
	canceled []string
}

func (c *postingCLOB) SubmitSignedOrder(ctx context.Context, signed clobclient.SignedOrderV2, tif clobclient.OrderType, postOnly bool) (*clobclient.OrderResponse, error) {
	if c.posted != nil {
		close(c.posted)
		c.posted = nil
	}
	return c.fakeCLOB.SubmitSignedOrder(ctx, signed, tif, postOnly)
}

func (c *postingCLOB) CancelOrderResult(_ context.Context, orderID string) (*clobclient.CancelResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.canceled = append(c.canceled, orderID)
	if c.accept {
		return &clobclient.CancelResult{Canceled: []string{orderID}}, nil
	}
	return &clobclient.CancelResult{NotCanceled: map[string]string{orderID: "order not found"}}, nil
}

func newConcurrentFixture(t *testing.T, response *clobclient.OrderResponse, submitErr error) (*Executor, *gatedStore, *postingCLOB, *[]error) {
	t.Helper()
	storer := &gatedStore{fakeStore: &fakeStore{inserted: true}}
	client := &postingCLOB{fakeCLOB: &fakeCLOB{response: response, submitErr: submitErr}}
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	reported := &[]error{}
	exec.SetErrorHandler(func(err error) {
		mu.Lock()
		defer mu.Unlock()
		*reported = append(*reported, err)
	})
	return exec, storer, client, reported
}

// shortCancelOpen is an open whose orphan cancel gives up quickly.
func shortCancelOpen() protocol.ExecutionOpenRequest {
	req := openRequest()
	req.Policy.CancelTimeoutMillis = 100
	return req
}

func hasOrphan(errs []error) bool {
	for _, err := range errs {
		if errors.Is(err, ErrOrphanOrder) {
			return true
		}
	}
	return false
}

func TestPostStartsBeforeCommitAndAnswerWaitsForIt(t *testing.T) {
	exec, storer, client, _ := newConcurrentFixture(t, &clobclient.OrderResponse{Success: true, OrderID: "order-1"}, nil)
	storer.release = make(chan struct{})
	posted := make(chan struct{})
	client.posted = posted
	done := make(chan error, 1)
	go func() { done <- exec.ExecuteOpen(context.Background(), openRequest()) }()
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("POST waited for the commit")
	}
	select {
	case err := <-done:
		t.Fatalf("answer handled before the commit: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(storer.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if storer.order.State != statemachine.StateLive {
		t.Fatalf("order state = %s, want LIVE", storer.order.State)
	}
}

func TestCommitFailureWithRejectedPostReleasesWithoutCancel(t *testing.T) {
	rejected := &clobclient.APIError{StatusCode: 400, Method: "POST", Path: "/order", Body: []byte(`{"error":"invalid order"}`)}
	exec, storer, client, reported := newConcurrentFixture(t, nil, rejected)
	storer.persistErrs = []error{errors.New("db down"), errors.New("db down")}
	if err := exec.ExecuteOpen(context.Background(), shortCancelOpen()); err == nil {
		t.Fatal("want an error")
	}
	if len(storer.released) == 0 {
		t.Fatal("reservation not released")
	}
	if len(client.canceled) != 0 {
		t.Fatalf("cancels sent for a rejected order: %v", client.canceled)
	}
	if hasOrphan(*reported) {
		t.Fatal("a rejected order is not an orphan")
	}
}

func TestCommitFailureWithLivePostCancelsThenReleases(t *testing.T) {
	exec, storer, client, reported := newConcurrentFixture(t, &clobclient.OrderResponse{Success: true, OrderID: "order-1"}, nil)
	storer.persistErrs = []error{errors.New("db down"), errors.New("db down")}
	client.accept = true
	err := exec.ExecuteOpen(context.Background(), shortCancelOpen())
	if !errors.Is(err, ErrOrphanOrder) {
		t.Fatalf("err = %v, want ErrOrphanOrder", err)
	}
	if len(client.canceled) == 0 || client.canceled[0] == "" {
		t.Fatalf("orphan not cancelled by ID: %v", client.canceled)
	}
	if len(storer.released) != 1 {
		t.Fatalf("released = %v, want the reservation once the cancel is accepted", storer.released)
	}
	if !hasOrphan(*reported) {
		t.Fatal("orphan not reported")
	}
}

func TestOrphanKeepsReservationWhenCancelNeverAccepted(t *testing.T) {
	exec, storer, client, reported := newConcurrentFixture(t, &clobclient.OrderResponse{Success: true, OrderID: "order-1"}, nil)
	storer.persistErrs = []error{errors.New("db down"), errors.New("db down")}
	err := exec.ExecuteOpen(context.Background(), shortCancelOpen())
	if !errors.Is(err, ErrOrphanOrder) {
		t.Fatalf("err = %v, want ErrOrphanOrder", err)
	}
	if len(client.canceled) == 0 {
		t.Fatal("no cancel attempted")
	}
	if len(storer.released) != 0 {
		t.Fatalf("reservation released while the order may be live: %v", storer.released)
	}
	if !hasOrphan(*reported) {
		t.Fatal("orphan not reported")
	}
}

func TestCommitRetryDuplicateCountsAsStored(t *testing.T) {
	exec, storer, _, reported := newConcurrentFixture(t, &clobclient.OrderResponse{Success: true, OrderID: "order-1"}, nil)
	storer.persistErrs = []error{errors.New("connection reset")}
	// The first commit "failed" after landing: the retry finds the row.
	storer.fakeStore.order = store.SignedOrderRecord{}
	dup := &dupOnRetryStore{gatedStore: storer}
	exec.store = dup
	if err := exec.ExecuteOpen(context.Background(), openRequest()); err != nil {
		t.Fatal(err)
	}
	if hasOrphan(*reported) {
		t.Fatal("a stored order reported as orphan")
	}
	if storer.order.State != statemachine.StateLive {
		t.Fatalf("order state = %s, want LIVE", storer.order.State)
	}
}

// dupOnRetryStore stores the order on the first, failing commit, so the
// retry reports a duplicate.
type dupOnRetryStore struct {
	*gatedStore
	calls int
}

func (s *dupOnRetryStore) PersistSubmittingOrder(ctx context.Context, record store.SignedOrderRecord, reason string) (store.SignedOrderRecord, error) {
	s.calls++
	if s.calls == 1 {
		if _, err := s.fakeStore.PersistSubmittingOrder(ctx, record, reason); err != nil {
			return store.SignedOrderRecord{}, err
		}
		return store.SignedOrderRecord{}, errors.New("connection reset")
	}
	return store.SignedOrderRecord{}, store.ErrDuplicate
}

func TestStreamEventBeforeCommitReplaysAfterIt(t *testing.T) {
	exec, storer, client, _ := newConcurrentFixture(t, &clobclient.OrderResponse{Success: true, OrderID: "order-1"}, nil)
	pending := accountfeed.NewPendingOrders(context.Background(), nil)
	exec.SetPendingOrders(pending)
	storer.release = make(chan struct{})
	posted := make(chan struct{})
	client.posted = posted
	done := make(chan error, 1)
	go func() { done <- exec.ExecuteOpen(context.Background(), openRequest()) }()
	<-posted
	// The stream reports the order while its row is still committing.
	var orderID string
	exec.inflight.mu.Lock()
	for id := range exec.inflight.posts {
		orderID = id
	}
	exec.inflight.mu.Unlock()
	replayed := make(chan struct{})
	if !pending.Hold(orderID, func(context.Context) { close(replayed) }) {
		t.Fatalf("order %q not expected while its commit runs", orderID)
	}
	close(storer.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-replayed:
	default:
		t.Fatal("held event not replayed once the row committed")
	}
}
