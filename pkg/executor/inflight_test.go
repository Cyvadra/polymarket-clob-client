package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// cancelProbeCLOB answers single-shot cancels the way the exchange does while
// a POST is in flight: refused until accepted is set.
type cancelProbeCLOB struct {
	*fakeCLOB
	mu       sync.Mutex
	probes   []string
	accepted bool
}

func (c *cancelProbeCLOB) CancelOrderResult(_ context.Context, orderID string) (*clobclient.CancelResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probes = append(c.probes, orderID)
	if c.accepted {
		return &clobclient.CancelResult{Canceled: []string{orderID}}, nil
	}
	return &clobclient.CancelResult{NotCanceled: map[string]string{orderID: "order is pending"}}, nil
}

// CancelOrder records an ordinary cancel under the probe's lock, since
// follow-up cancels run on their own goroutines.
func (c *cancelProbeCLOB) CancelOrder(ctx context.Context, orderID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fakeCLOB.CancelOrder(ctx, orderID)
}

func (c *cancelProbeCLOB) ordinaryCancels() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.canceledOrderIDs)
}

func (c *cancelProbeCLOB) probeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.probes)
}

func (c *cancelProbeCLOB) accept() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accepted = true
}

func laneWithSubmittingOpen() *fakeStore {
	return &fakeStore{
		inserted: true,
		order:    store.SignedOrderRecord{IntentID: "open-1", ChildSequence: 1, ExchangeOrderID: "order-open", State: statemachine.StateSubmitting, Revision: 1},
		intent:   store.OrderIntentRecord{IntentID: "open-1", UniqueTag: "lane-a", ConditionID: "condition", TokenID: "token", Kind: store.IntentOpen},
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A CANCEL_OPEN for an order whose POST is in flight fires cancels at its
// signed order ID at once and keeps firing until the POST is answered, then
// stops; it does not wait for the answer while holding the lane.
func TestCancelOpenFiresAtAnInFlightPostUntilItIsAnswered(t *testing.T) {
	client := &cancelProbeCLOB{fakeCLOB: &fakeCLOB{}}
	exec, err := New(laneWithSubmittingOpen(), client, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	post := exec.inflight.start("order-open")
	if err := exec.ExecuteClose(context.Background(), cancelOpenRequest()); err != nil {
		t.Fatalf("execute cancel open: %v", err)
	}
	if !post.cancelWanted.Load() {
		t.Fatal("the in-flight POST must be marked for cancel")
	}
	waitFor(t, "repeated cancels", func() bool { return client.probeCount() >= 3 })
	exec.inflight.finish("order-open", post)
	time.Sleep(3 * inflightCancelInterval)
	settled := client.probeCount()
	time.Sleep(3 * inflightCancelInterval)
	if client.probeCount() != settled {
		t.Fatalf("cancels kept firing after the POST was answered: %d then %d", settled, client.probeCount())
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, id := range client.probes {
		if id != "order-open" {
			t.Fatalf("cancel sent for %q, want the signed order ID", id)
		}
	}
	if len(client.canceledOrderIDs) != 0 {
		t.Fatalf("no ordinary cancel is due before the POST answer is recorded, got %v", client.canceledOrderIDs)
	}
}

// Firing stops as soon as the exchange accepts one cancel, and the poster
// then has nothing left to cancel.
func TestInFlightCancelStopsOnceAccepted(t *testing.T) {
	client := &cancelProbeCLOB{fakeCLOB: &fakeCLOB{}}
	storer := laneWithSubmittingOpen()
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	post := exec.inflight.start("order-open")
	client.accept()
	if err := exec.ExecuteClose(context.Background(), cancelOpenRequest()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "an accepted cancel", post.canceled.Load)
	time.Sleep(3 * inflightCancelInterval)
	if n := client.probeCount(); n > 2 {
		t.Fatalf("expected firing to stop after the accepted cancel, got %d sends", n)
	}
	exec.inflight.finish("order-open", post)
	storer.order.State = statemachine.StateLive
	exec.finishInflightCancel(context.Background(), protocol.ExecutionIntent{IntentID: "open-1"}, 1, post)
	if len(client.canceledOrderIDs) != 0 {
		t.Fatalf("an accepted in-flight cancel needs no follow-up, got %v", client.canceledOrderIDs)
	}
}

// Once the POST is answered, an order it left working is cancelled the
// ordinary way; one it left matched is not.
func TestCancelAfterPostAnswerFollowsTheAnsweredState(t *testing.T) {
	for _, tc := range []struct {
		state      statemachine.State
		wantCancel bool
	}{
		{statemachine.StateLive, true},
		{statemachine.StatePartiallyFilled, true},
		{statemachine.StateFilled, false},
		{statemachine.StateRejected, false},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			client := &cancelProbeCLOB{fakeCLOB: &fakeCLOB{}}
			storer := laneWithSubmittingOpen()
			storer.order.State = tc.state
			exec, err := New(storer, client, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			post := &inflightPost{done: make(chan struct{})}
			post.cancelWanted.Store(true)
			exec.finishInflightCancel(context.Background(), protocol.ExecutionIntent{IntentID: "open-1"}, 1, post)
			got := len(client.canceledOrderIDs) == 1 && client.canceledOrderIDs[0] == "order-open"
			if got != tc.wantCancel {
				t.Fatalf("cancel sent = %v, want %v (%v)", got, tc.wantCancel, client.canceledOrderIDs)
			}
			if tc.wantCancel && storer.order.State != statemachine.StateCancelPending {
				t.Fatalf("expected CANCEL_PENDING, got %s", storer.order.State)
			}
		})
	}
}

// A SUBMITTING order this process is not posting (another instance, or one
// left by a restart) is cancelled by its signed order ID until the exchange
// accepts, then no more.
func TestCancelOpenRetriesASubmittingOrderPostedElsewhereUntilAccepted(t *testing.T) {
	client := &cancelProbeCLOB{fakeCLOB: &fakeCLOB{}}
	exec, err := New(laneWithSubmittingOpen(), client, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.ExecuteClose(context.Background(), cancelOpenRequest()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "repeated cancels", func() bool { return client.probeCount() >= 3 })
	client.accept()
	time.Sleep(3 * inflightCancelInterval)
	settled := client.probeCount()
	time.Sleep(3 * inflightCancelInterval)
	if client.probeCount() != settled {
		t.Fatalf("cancels kept going after one was accepted: %d then %d", settled, client.probeCount())
	}
}

// A POST that ended unknown may still have booked the order, so a cancel
// wanted during it keeps going after the answer until the exchange accepts.
func TestCancelAfterUnknownPostRetriesUntilAccepted(t *testing.T) {
	client := &cancelProbeCLOB{fakeCLOB: &fakeCLOB{}}
	storer := laneWithSubmittingOpen()
	storer.order.State = statemachine.StateSubmitUnknown
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	post := &inflightPost{done: make(chan struct{})}
	post.cancelWanted.Store(true)
	finished := make(chan struct{})
	go func() {
		exec.finishInflightCancel(context.Background(), protocol.ExecutionIntent{IntentID: "open-1"}, 1, post)
		close(finished)
	}()
	waitFor(t, "repeated cancels", func() bool { return client.probeCount() >= 3 })
	client.accept()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel retries did not stop once accepted")
	}
}

// A CANCEL_OPEN that lands after the poster took its answer still gets the
// follow-up cancel, even if the poster read cancelWanted before it was set.
func TestCancelOpenAfterThePostIsAnsweredStillCancels(t *testing.T) {
	client := &cancelProbeCLOB{fakeCLOB: &fakeCLOB{}}
	storer := laneWithSubmittingOpen()
	exec, err := New(storer, client, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	post := exec.inflight.start("order-open")
	storer.order.State = statemachine.StateLive
	// The poster has taken its answer and seen no cancel wanted.
	exec.finishInflightCancel(context.Background(), protocol.ExecutionIntent{IntentID: "open-1"}, 1, post)
	exec.wantInflightCancel(context.Background(), protocol.ExecutionPolicy{}, "open-1", 1, "order-open", post)
	waitFor(t, "the follow-up cancel", func() bool { return client.ordinaryCancels() == 1 })
}

// A CANCEL_OPEN repeated after the first one's firing gave up fires again.
func TestRepeatedCancelOpenFiresAgainAfterTheFirstGaveUp(t *testing.T) {
	client := &cancelProbeCLOB{fakeCLOB: &fakeCLOB{}}
	exec, err := New(laneWithSubmittingOpen(), client, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	post := exec.inflight.start("order-open")
	defer exec.inflight.finish("order-open", post)
	policy := protocol.ExecutionPolicy{CancelTimeoutMillis: 60}
	exec.wantInflightCancel(context.Background(), policy, "open-1", 1, "order-open", post)
	waitFor(t, "the first firing to give up", func() bool { return !post.firing.Load() && client.probeCount() > 0 })
	before := client.probeCount()
	exec.wantInflightCancel(context.Background(), policy, "open-1", 1, "order-open", post)
	waitFor(t, "a second firing", func() bool { return client.probeCount() > before })
}

// A cancel the exchange refuses for a live order is an error, not an
// acceptance; one refused because the order is already gone is not.
func TestCancelOpenOrderReadsTheExchangeRefusal(t *testing.T) {
	order := store.SignedOrderRecord{IntentID: "open-1", ChildSequence: 1, ExchangeOrderID: "order-open", State: statemachine.StateLive, Revision: 1}
	for _, tc := range []struct {
		reason  string
		wantErr bool
	}{
		{"order is pending", true},
		{"order can't be found - already canceled or matched", false},
	} {
		client := &fakeCLOB{cancelErrs: []error{&clobclient.CancelRefusedError{OrderID: "order-open", Reason: tc.reason}}}
		storer := laneWithSubmittingOpen()
		storer.order = order
		exec, err := New(storer, client, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		err = exec.cancelOpenOrder(context.Background(), protocol.ExecutionPolicy{}, order, cancelRequested)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%q: err = %v, want error %v", tc.reason, err, tc.wantErr)
		}
	}
}

// An open withdrawn before it was worked reports a canceled result, so the
// strategy learns the request ended without an order.
func TestDropOpenPublishesACanceledResult(t *testing.T) {
	exec, err := New(&fakeStore{}, &fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	pub := &recordingPublisher{}
	exec.SetEventPublisher(pub)
	exec.DropOpen(protocol.ExecutionOpenRequest{RequestID: "req-1", Strategy: "strategy", UniqueTag: "lane-a", ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: protocol.SideBuy}, "canceled before submission")
	if len(pub.publishes) != 1 {
		t.Fatalf("expected one result, got %+v", pub.publishes)
	}
	result, ok := pub.publishes[0].value.(protocol.ExecutionOpenResult)
	if !ok || pub.publishes[0].subject != protocol.SubjectExecutionOpenResult || result.RequestID != "req-1" || result.Status != protocol.ResultFailed || result.ReasonCode != protocol.ReasonCanceled || result.UniqueTag != "lane-a" {
		t.Fatalf("unexpected result %+v", pub.publishes[0])
	}
}
