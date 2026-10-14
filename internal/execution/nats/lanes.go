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
	work func(T)

	mu      sync.Mutex
	lanes   map[string]*laneQueue[T]
	running sync.WaitGroup
}

type laneQueue[T any] struct {
	pending []T
}

func newLaneWorkers[T any](collapse bool, limit int, skip func(T) bool, work func(T)) *laneWorkers[T] {
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
		w.mu.Unlock()

		if w.skip != nil && w.skip(item) {
			continue
		}
		if w.slots != nil {
			w.slots <- struct{}{}
		}
		w.work(item)
		if w.slots != nil {
			<-w.slots
		}
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
