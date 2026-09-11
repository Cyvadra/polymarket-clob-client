package nats

import (
	"context"
	"fmt"
	"sync"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

// CloseExecutor is the execution surface that consumes close commands.
type CloseExecutor interface {
	ExecuteClose(context.Context, protocol.ExecutionCloseRequest) error
}

// SubscribeClose wires the strategy.execution.close subject to the executor.
//
// A close now retries a sell the exchange refused for missing balance for
// several seconds while the buy that funded it settles. The NATS client
// dispatches one subscription's messages serially, so running ExecuteClose
// inline would make every lane's closes wait behind one lane's retry. The
// subscription handler instead hands each close to a per-lane worker and
// returns at once; closes on different lanes then run concurrently while
// closes on one lane still run in order. onError receives an ExecuteClose
// failure, since the handler has already returned by the time it happens.
func SubscribeClose(ctx context.Context, bus Subscriber, execution CloseExecutor, onError func(error)) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if bus == nil || execution == nil {
		return fmt.Errorf("NATS subscriber and close executor are required")
	}
	dispatcher := newCloseDispatcher(ctx, execution, onError)
	return bus.Subscribe(protocol.SubjectStrategyExecutionClose, dispatcher.handle)
}

// closeDispatcher fans strategy.execution.close commands out to one worker
// goroutine per lane. A lane keeps only its most recent pending close:
// executiond treats a newer close on a lane as a replacement of any still
// pending one, so a burst on a single lane collapses to at most the running
// close plus the latest queued.
type closeDispatcher struct {
	ctx     context.Context
	exec    CloseExecutor
	onError func(error)

	mu    sync.Mutex
	lanes map[string]*closeLane
}

type closeLane struct {
	mu      sync.Mutex
	pending *protocol.ExecutionCloseRequest
	running bool
}

func newCloseDispatcher(ctx context.Context, exec CloseExecutor, onError func(error)) *closeDispatcher {
	return &closeDispatcher{ctx: ctx, exec: exec, onError: onError, lanes: make(map[string]*closeLane)}
}

func (d *closeDispatcher) handle(_ context.Context, payload []byte) error {
	request, err := natsbus.DecodeJSON[protocol.ExecutionCloseRequest](payload)
	if err != nil {
		return err
	}
	d.enqueue(request)
	return nil
}

// closeLaneKey groups closes the same way the executor's lane lock does, so
// the dispatcher serializes exactly the closes that would contend on it.
func closeLaneKey(req protocol.ExecutionCloseRequest) string {
	return req.Strategy + "\x00" + req.UniqueTag + "\x00" + req.ConditionID + "\x00" + req.AssetID
}

func (d *closeDispatcher) enqueue(req protocol.ExecutionCloseRequest) {
	key := closeLaneKey(req)
	d.mu.Lock()
	lane := d.lanes[key]
	if lane == nil {
		lane = &closeLane{}
		d.lanes[key] = lane
	}
	lane.mu.Lock()
	lane.pending = &req
	start := !lane.running
	lane.running = true
	lane.mu.Unlock()
	d.mu.Unlock()
	if start {
		go d.run(key, lane)
	}
}

func (d *closeDispatcher) run(key string, lane *closeLane) {
	for {
		lane.mu.Lock()
		next := lane.pending
		lane.pending = nil
		lane.mu.Unlock()

		if next == nil {
			// Nothing left to run. Retire the lane while holding both locks so
			// a concurrent enqueue either still finds this lane and revives it,
			// or misses it and creates a fresh one, but never starts a second
			// worker for the same lane.
			d.mu.Lock()
			lane.mu.Lock()
			if lane.pending == nil {
				lane.running = false
				if d.lanes[key] == lane {
					delete(d.lanes, key)
				}
				lane.mu.Unlock()
				d.mu.Unlock()
				return
			}
			lane.mu.Unlock()
			d.mu.Unlock()
			continue
		}

		if err := d.exec.ExecuteClose(d.ctx, *next); err != nil && d.onError != nil {
			d.onError(fmt.Errorf("handle NATS subject %s: %w", protocol.SubjectStrategyExecutionClose, err))
		}
	}
}

// idle reports whether every lane worker has finished. It exists for tests,
// which drive the handler directly and need to wait for the async workers.
func (d *closeDispatcher) idle() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.lanes) == 0
}
