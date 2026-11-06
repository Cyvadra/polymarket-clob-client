package nats

import (
	"context"
	"sync"
)

// Drainer is a dispatcher whose in-flight work shutdown waits for.
type Drainer interface {
	// Wait blocks until every queued and running item has finished, or ctx
	// is done.
	Wait(ctx context.Context) error
}

// laneWorkers runs work items on one goroutine per lane: items on one lane
// run in order, items on different lanes run concurrently. The NATS client
// delivers a subscription's messages serially, so handing each message to a
// lane lets the delivery goroutine return at once.
type laneWorkers[T any] struct {
	// collapse keeps only a lane's newest pending item, for commands where a
	// newer one replaces any still waiting. Otherwise every item runs.
	collapse bool
	// slots, when non-nil, bounds how many items run at once across lanes.
	slots chan struct{}
	// skip reports whether a queued item should be dropped rather than run,
	// such as once shutdown has begun.
	skip func(T) bool
	// work runs one item. posting is the item's gate: see laneTicket.
	work func(item T, posting func() bool)

	mu      sync.Mutex
	lanes   map[string]*laneQueue[T]
	running sync.WaitGroup
}

type laneQueue[T any] struct {
	pending []T
	// current is the item taken off pending and not yet finished.
	current *laneTicket[T]
}

// laneTicket tracks an item between leaving pending and finishing, so drop
// can still reach it there. Until it starts (it may wait for a slot) drop
// withdraws it like a pending item. Once started, drop marks it instead, and
// the item learns of the mark through posting: work calls posting at its point
// of no return (for an open, just before the POST, once its order can be found
// and cancelled), and posting reports whether a drop came in the meantime.
// After posting, drop leaves the item alone. Fields are guarded by the
// workers' mu.
type laneTicket[T any] struct {
	item      T
	started   bool
	withdrawn bool
	marked    bool
	posted    bool
}

func newLaneWorkers[T any](collapse bool, limit int, skip func(T) bool, work func(item T, posting func() bool)) *laneWorkers[T] {
	w := &laneWorkers[T]{collapse: collapse, skip: skip, work: work, lanes: make(map[string]*laneQueue[T])}
	if limit > 0 {
		w.slots = make(chan struct{}, limit)
	}
	return w
}

func (w *laneWorkers[T]) enqueue(key string, item T) {
	w.mu.Lock()
	defer w.mu.Unlock()
	lane := w.lanes[key]
	if lane != nil {
		if w.collapse {
			lane.pending = lane.pending[:0]
		}
		lane.pending = append(lane.pending, item)
		return
	}
	// A lane exists exactly while its worker runs, so a missing lane means
	// no worker: start one.
	lane = &laneQueue[T]{pending: []T{item}}
	w.lanes[key] = lane
	w.running.Add(1)
	go w.run(key, lane)
}

// drop removes and returns the items still waiting on a lane, including one
// taken off the queue that has not started. The item its worker is running,
// if any, is not among them and runs to completion; if it has not yet called
// posting, it is marked so posting reports the drop.
func (w *laneWorkers[T]) drop(key string) []T {
	w.mu.Lock()
	defer w.mu.Unlock()
	lane := w.lanes[key]
	if lane == nil {
		return nil
	}
	var dropped []T
	if ticket := lane.current; ticket != nil {
		switch {
		case !ticket.started && !ticket.withdrawn:
			ticket.withdrawn = true
			dropped = append(dropped, ticket.item)
		case ticket.started && !ticket.posted:
			ticket.marked = true
		}
	}
	dropped = append(dropped, lane.pending...)
	lane.pending = lane.pending[:0]
	if len(dropped) == 0 {
		return nil
	}
	return dropped
}

// runTicket runs one item taken off lane unless drop withdraws it first.
func (w *laneWorkers[T]) runTicket(lane *laneQueue[T], ticket *laneTicket[T]) {
	if w.skip != nil && w.skip(ticket.item) {
		return
	}
	if w.slots != nil {
		w.slots <- struct{}{}
		defer func() { <-w.slots }()
	}
	w.mu.Lock()
	if ticket.withdrawn {
		w.mu.Unlock()
		return
	}
	ticket.started = true
	w.mu.Unlock()
	w.work(ticket.item, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		ticket.posted = true
		return ticket.marked
	})
}

// laneKey names one position lane: a strategy's unique tag on one outcome
// token. Opens and closes on a lane share it, so a close can find the opens
// queued for the position it acts on.
func laneKey(strategy, uniqueTag, conditionID, tokenID string) string {
	return strategy + "\x00" + uniqueTag + "\x00" + conditionID + "\x00" + tokenID
}

func (w *laneWorkers[T]) run(key string, lane *laneQueue[T]) {
	defer w.running.Done()
	for {
		// Retiring the lane under the same lock enqueue takes means an
		// enqueue either finds this lane and its item is run here, or finds
		// none and starts a fresh worker; never both, never neither.
		w.mu.Lock()
		if len(lane.pending) == 0 {
			delete(w.lanes, key)
			w.mu.Unlock()
			return
		}
		item := lane.pending[0]
		lane.pending = lane.pending[1:]
		ticket := &laneTicket[T]{item: item}
		lane.current = ticket
		w.mu.Unlock()

		w.runTicket(lane, ticket)

		w.mu.Lock()
		lane.current = nil
		w.mu.Unlock()
	}
}

func (w *laneWorkers[T]) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		w.running.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// idle reports whether every lane worker has finished. It exists for tests,
// which drive the handler directly and need to wait for the async workers.
func (w *laneWorkers[T]) idle() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.lanes) == 0
}
