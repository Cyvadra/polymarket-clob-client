package nats

import (
	"context"
	"fmt"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

// CloseExecutor is the execution surface that consumes close commands.
type CloseExecutor interface {
	ExecuteClose(context.Context, protocol.ExecutionCloseRequest) error
}

// LaneHolder is implemented by a CloseExecutor that can tell whether this
// wallet holds a position on a close's lane.
type LaneHolder interface {
	HoldsLane(context.Context, protocol.ExecutionCloseRequest) (bool, error)
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
//
// A close for a strategy outside the allowlist belongs to another executiond
// on the bus and is dropped silently, as an open is, unless the executor
// reports this wallet still holds the lane: a strategy taken off the list
// must still be able to close what it opened here.
//
// opens, when non-nil, is the open dispatcher: a CANCEL_OPEN first withdraws
// the opens still queued on its lane, which have no order yet for the
// executor to cancel.
func SubscribeClose(ctx context.Context, bus Subscriber, execution CloseExecutor, allowed *Allowlist, opens OpenQueue, onError func(error)) (Drainer, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if bus == nil || execution == nil || allowed == nil {
		return nil, fmt.Errorf("NATS subscriber, close executor, and strategy allowlist are required")
	}
	dispatcher := newCloseDispatcher(ctx, execution, allowed, onError)
	dispatcher.opens = opens
	if err := bus.Subscribe(protocol.SubjectStrategyExecutionClose, dispatcher.handle); err != nil {
		return nil, err
	}
	return dispatcher, nil
}

// closeDispatcher fans strategy.execution.close commands out to one worker
// goroutine per lane. A lane keeps only its most recent pending close:
// executiond treats a newer close on a lane as a replacement of any still
// pending one, so a burst on a single lane collapses to at most the running
// close plus the latest queued.
type closeDispatcher struct {
	*laneWorkers[protocol.ExecutionCloseRequest]
	ctx     context.Context
	exec    CloseExecutor
	allowed *Allowlist
	opens   OpenQueue
	onError func(error)
}

func newCloseDispatcher(ctx context.Context, exec CloseExecutor, allowed *Allowlist, onError func(error)) *closeDispatcher {
	d := &closeDispatcher{ctx: ctx, exec: exec, allowed: allowed, onError: onError}
	d.laneWorkers = newLaneWorkers(true, 0, nil, d.execute)
	return d
}

func (d *closeDispatcher) handle(_ context.Context, payload []byte) error {
	request, err := natsbus.DecodeJSON[protocol.ExecutionCloseRequest](payload)
	if err != nil {
		return err
	}
	if _, ok := d.exec.(LaneHolder); !ok && !d.allowed.Allows(request.Strategy) {
		return nil
	}
	// Withdraw the lane's queued opens here, on the delivery goroutine and
	// before the cancel is queued, so no open that arrived ahead of the
	// cancel can be posted after it has run. Opens for strategies outside
	// the allowlist were never queued, so nothing is dropped for them.
	if request.Mode == protocol.ExecutionCloseModeCancelOpen && d.opens != nil {
		d.opens.DropLane(closeLaneKey(request))
	}
	d.enqueue(request)
	return nil
}

func (d *closeDispatcher) enqueue(req protocol.ExecutionCloseRequest) {
	d.laneWorkers.enqueue(closeLaneKey(req), req)
}

// closeLaneKey groups closes the same way the executor's lane lock does, so
// the dispatcher serializes exactly the closes that would contend on it.
func closeLaneKey(req protocol.ExecutionCloseRequest) string {
	return laneKey(req.Strategy, req.UniqueTag, req.ConditionID, req.AssetID)
}

func (d *closeDispatcher) execute(next protocol.ExecutionCloseRequest, _ func() bool) {
	// The lane lookup runs here, off the NATS delivery goroutine, since
	// every close for every other wallet on the bus reaches this check.
	if !d.allowed.Allows(next.Strategy) {
		held, err := d.exec.(LaneHolder).HoldsLane(d.ctx, next)
		if err != nil && d.onError != nil {
			d.onError(fmt.Errorf("check lane of close for strategy %q: %w", next.Strategy, err))
		}
		if err != nil || !held {
			return
		}
	}
	if err := d.exec.ExecuteClose(d.ctx, next); err != nil && d.onError != nil {
		d.onError(fmt.Errorf("handle NATS subject %s: %w", protocol.SubjectStrategyExecutionClose, err))
	}
}
