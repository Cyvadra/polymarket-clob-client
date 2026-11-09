package executor

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
)

// A ladder's levels go out in one POST /orders, and the levels that crossed
// are reposted together, not one call each.
func TestLadderPostsItsLevelsInOneBatch(t *testing.T) {
	h := newLadderHarness(t)
	h.clob.cross[0.60], h.clob.cross[0.59] = 1, 1
	if err := h.exec.Execute(context.Background(), ladderIntent("i")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "every level to rest", func() bool { return countStates(h.store.states("i"), statemachine.StateLive) == 11 })
	h.clob.mu.Lock()
	batches := slices.Clone(h.clob.batchPosts)
	h.clob.mu.Unlock()
	if !slices.Equal(batches, []int{11, 2}) {
		t.Fatalf("batch posts %v, want the 11 levels together, then the 2 crossed ones", batches)
	}
}

// A ladder wider than one batch is split at clobclient.MaxBatchOrders.
func TestLadderSplitsLevelsIntoBatchesOfFifteen(t *testing.T) {
	h := newLadderHarness(t)
	intent := ladderIntent("i")
	intent.BottomPrice, intent.TargetUSD = "0.41", "20"
	if err := h.exec.Execute(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	eventually(t, "every level to rest", func() bool { return countStates(h.store.states("i"), statemachine.StateLive) == 20 })
	h.clob.mu.Lock()
	batches := slices.Clone(h.clob.batchPosts)
	h.clob.mu.Unlock()
	slices.Sort(batches)
	if !slices.Equal(batches, []int{5, 15}) {
		t.Fatalf("batch posts %v, want one of 15 and one of 5", batches)
	}
}

// Stopping a ladder cancels its resting levels in one DELETE /orders.
func TestStoppedLadderCancelsRestingLevelsInOneBatch(t *testing.T) {
	h := newLadderHarness(t)
	h.clob.forever(0.60)
	if err := h.exec.Execute(context.Background(), ladderIntent("i")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the crossed level to be retried", func() bool { return h.clob.postCount(0.60) >= 2 })
	lane, err := h.exec.laneActiveOrders(context.Background(), "cond", "token", "lane")
	if err != nil {
		t.Fatal(err)
	}
	// The close path gathers a ladder's levels and cancels them together.
	ladders := laneLadders{}
	for _, candidate := range lane {
		ladders.add(candidate)
	}
	if err := h.exec.cancelLaneLadders(context.Background(), ladders, map[string]bool{}, "test"); err != nil {
		t.Fatal(err)
	}
	h.clob.mu.Lock()
	defer h.clob.mu.Unlock()
	if !slices.Equal(h.clob.batchCancels, []int{10}) || len(h.clob.cancels) != 10 {
		t.Fatalf("batch cancels %v and %d cancels, want one batch of the 10 resting levels", h.clob.batchCancels, len(h.clob.cancels))
	}
}

// An exchange without the batch endpoints answers 404: the orders go one by
// one, then and from then on.
func TestBatchFallsBackToSingleCallsWhenTheEndpointIsMissing(t *testing.T) {
	h := newLadderHarness(t)
	h.clob.noBatch = true
	if err := h.exec.Execute(context.Background(), ladderIntent("i")); err != nil {
		t.Fatal(err)
	}
	if got := countStates(h.store.states("i"), statemachine.StateLive); got != 11 {
		t.Fatalf("%d levels rest, want 11 posted one by one", got)
	}
	if err := h.exec.Execute(context.Background(), ladderIntent("j")); err != nil {
		t.Fatal(err)
	}
	h.clob.mu.Lock()
	defer h.clob.mu.Unlock()
	if len(h.clob.batchPosts) != 1 {
		t.Fatalf("batch posts %v, want only the first try before falling back", h.clob.batchPosts)
	}
}

func TestBatchSendsWhenTheLastMemberJoinsOrLeaves(t *testing.T) {
	var calls atomic.Int32
	var sent []int
	b := newBatch(3, func(_ context.Context, reqs []int) ([]int, []error) {
		calls.Add(1)
		sent = reqs
		out := make([]int, len(reqs))
		for i, r := range reqs {
			out[i] = r * 10
		}
		return out, nil
	})
	a, c, d := b.member(), b.member(), b.member()
	got := make(chan int, 2)
	go func() { r, _, _ := a.call(context.Background(), 1); got <- r }()
	go func() { r, _, _ := c.call(context.Background(), 2); got <- r }()
	d.leave()
	if x, y := <-got, <-got; x+y != 30 {
		t.Fatalf("answers %d and %d, want 10 and 20", x, y)
	}
	if calls.Load() != 1 || len(sent) != 2 {
		t.Fatalf("%d calls carrying %v, want one call of the two joined", calls.Load(), sent)
	}
	// A member already used goes out on its own instead of joining again.
	if _, _, joined := a.call(context.Background(), 3); joined {
		t.Fatal("a used member joined a sent batch")
	}

	empty := newBatch(1, func(context.Context, []int) ([]int, []error) {
		t.Fatal("a batch every member left must not send")
		return nil, nil
	})
	empty.member().leave()
}
