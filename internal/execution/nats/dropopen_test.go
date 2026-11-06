package nats

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
)

// droppingOpenExecutor is a gated open executor that records the opens
// reported as withdrawn.
type droppingOpenExecutor struct {
	*gatedOpenExecutor
	mu      sync.Mutex
	dropped []string
	reasons []string
}

func (d *droppingOpenExecutor) DropOpen(req protocol.ExecutionOpenRequest, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dropped = append(d.dropped, req.RequestID)
	d.reasons = append(d.reasons, reason)
}

func TestLaneWorkersDropKeepsTheRunningItem(t *testing.T) {
	release := make(chan struct{})
	started := make(chan int, 4)
	var mu sync.Mutex
	var ran []int
	w := newLaneWorkers(false, 0, nil, func(item int, _ func() bool) {
		started <- item
		<-release
		mu.Lock()
		ran = append(ran, item)
		mu.Unlock()
	})
	w.enqueue("a", 1)
	<-started
	w.enqueue("a", 2)
	w.enqueue("a", 3)
	w.enqueue("b", 4)
	<-started
	if dropped := w.drop("a"); !slices.Equal(dropped, []int{2, 3}) {
		t.Fatalf("dropped %v, want [2 3]", dropped)
	}
	if dropped := w.drop("missing"); dropped != nil {
		t.Fatalf("dropped %v from a lane with no worker", dropped)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	slices.Sort(ran)
	if !slices.Equal(ran, []int{1, 4}) {
		t.Fatalf("ran %v, want [1 4]", ran)
	}
}

// A CANCEL_OPEN withdraws the opens still queued on its lane, reporting each
// as canceled, so none of them posts after the cancel has run. The open
// already running and opens on other lanes are left alone.
func TestCancelOpenDropsQueuedOpensOnItsLane(t *testing.T) {
	exec := &droppingOpenExecutor{gatedOpenExecutor: newGatedOpenExecutor()}
	handler, work, _ := subscribeOpen(t, exec, testAllowlist(t))
	deliverOpen(t, handler, openOn("strategy", "lane-a", "running"))
	awaitEntered(t, exec.gatedOpenExecutor, 1)
	deliverOpen(t, handler, openOn("strategy", "lane-a", "queued-1"))
	deliverOpen(t, handler, openOn("strategy", "lane-a", "queued-2"))
	deliverOpen(t, handler, openOn("strategy", "lane-b", "other-lane"))
	awaitEntered(t, exec.gatedOpenExecutor, 1)

	closes := &fakeCloseExecutor{}
	subscriber := &fakeSubscriber{}
	closeWork, err := SubscribeClose(context.Background(), subscriber, closes, testAllowlist(t), work.(OpenQueue), nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(protocol.ExecutionCloseRequest{SchemaVersion: protocol.SchemaVersionV1, Strategy: "strategy", UniqueTag: "lane-a", ConditionID: "condition", AssetID: "token", Outcome: "Up", Mode: protocol.ExecutionCloseModeCancelOpen})
	if err != nil {
		t.Fatal(err)
	}
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatal(err)
	}

	close(exec.release)
	drain(t, work)
	drain(t, closeWork)
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if !slices.Equal(exec.dropped, []string{"queued-1", "queued-2"}) {
		t.Fatalf("dropped %v, want the two queued lane-a opens", exec.dropped)
	}
	for _, reason := range exec.reasons {
		if reason != droppedOpenReason {
			t.Fatalf("drop reason %q", reason)
		}
	}
	exec.gatedOpenExecutor.mu.Lock()
	ran := slices.Clone(exec.order)
	exec.gatedOpenExecutor.mu.Unlock()
	slices.Sort(ran)
	if !slices.Equal(ran, []string{"other-lane", "running"}) {
		t.Fatalf("ran %v, want only the running open and the other lane", ran)
	}
	closes.mu.Lock()
	defer closes.mu.Unlock()
	if len(closes.calls) != 1 {
		t.Fatalf("expected the cancel to still reach the executor, got %d calls", len(closes.calls))
	}
}

// Only CANCEL_OPEN withdraws queued opens: a LIMIT_CLOSE leaves an entry
// working.
func TestLimitCloseLeavesQueuedOpens(t *testing.T) {
	exec := &droppingOpenExecutor{gatedOpenExecutor: newGatedOpenExecutor()}
	handler, work, _ := subscribeOpen(t, exec, testAllowlist(t))
	deliverOpen(t, handler, openOn("strategy", "lane-a", "running"))
	awaitEntered(t, exec.gatedOpenExecutor, 1)
	deliverOpen(t, handler, openOn("strategy", "lane-a", "queued"))

	subscriber := &fakeSubscriber{}
	closeWork, err := SubscribeClose(context.Background(), subscriber, &fakeCloseExecutor{}, testAllowlist(t), work.(OpenQueue), nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(protocol.ExecutionCloseRequest{SchemaVersion: protocol.SchemaVersionV1, Strategy: "strategy", UniqueTag: "lane-a", ConditionID: "condition", AssetID: "token", Outcome: "Up", Mode: protocol.ExecutionCloseModeLimit, LimitPrice: "0.5"})
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	close(exec.release)
	drain(t, work)
	drain(t, closeWork)
	if len(exec.dropped) != 0 {
		t.Fatalf("a LIMIT_CLOSE dropped %v", exec.dropped)
	}
}

// An item taken off its lane but still waiting for a slot is withdrawn by a
// drop, like a queued one, and never runs.
func TestLaneWorkersDropWithdrawsAnItemWaitingForASlot(t *testing.T) {
	release := make(chan struct{})
	started := make(chan string, 4)
	w := newLaneWorkers(false, 1, nil, func(item string, _ func() bool) {
		started <- item
		if item == "holder" {
			<-release
		}
	})
	w.enqueue("a", "holder")
	if got := <-started; got != "holder" {
		t.Fatalf("started %q", got)
	}
	w.enqueue("b", "waiting")
	waitUntil(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		lane := w.lanes["b"]
		return lane != nil && lane.current != nil && len(lane.pending) == 0
	})
	if dropped := w.drop("b"); !slices.Equal(dropped, []string{"waiting"}) {
		t.Fatalf("dropped %v, want [waiting]", dropped)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	close(started)
	for item := range started {
		if item == "waiting" {
			t.Fatal("a withdrawn item ran")
		}
	}
}

// A drop that reaches a running item before it posts is reported through
// posting; one after it posts is not.
func TestLaneWorkersPostingReportsADropWhileRunning(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dropBefore bool
		want       bool
	}{
		{"drop before posting", true, true},
		{"drop after posting", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			proceed := make(chan struct{})
			result := make(chan bool, 1)
			w := newLaneWorkers(false, 0, nil, func(_ string, posting func() bool) {
				close(entered)
				<-proceed
				result <- posting()
			})
			w.enqueue("a", "open")
			<-entered
			if tc.dropBefore {
				if dropped := w.drop("a"); dropped != nil {
					t.Fatalf("a running item must not be withdrawn, got %v", dropped)
				}
			}
			close(proceed)
			if got := <-result; got != tc.want {
				t.Fatalf("posting() = %v, want %v", got, tc.want)
			}
			w.drop("a")
		})
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
