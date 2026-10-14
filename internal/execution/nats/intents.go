// Package nats contains NATS adapters for executiond's internal handlers.
package nats

import (
	"context"
	"fmt"
	"strings"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

type Subscriber interface {
	Subscribe(string, natsbus.Handler) error
}

// MaxConcurrentOpens bounds how many opens are worked at once, across lanes.
const MaxConcurrentOpens = 4

// OpenExecutor is the execution surface that consumes open commands.
type OpenExecutor interface {
	ExecuteOpen(context.Context, protocol.ExecutionOpenRequest) error
}

// SubscribeOpen wires the strategy.execution.open subject to the executor.
// An open for a strategy outside the allowlist belongs to another executiond
// on the bus. It is dropped without a result or a log line: a failure result
// would read to the strategy as the owning wallet refusing the order. An open
// with no strategy belongs to no wallet, so every instance answers it with
// the validation failure.
//
// The NATS client dispatches one subscription's messages serially, and an
// open returns only after its POST is answered, so run inline every open
// would wait out the exchange round trip of each one ahead of it. Opens are
// instead handed to one worker per lane, as closes are: opens on one lane
// still run in the order they arrived, so a top-up never overtakes the entry
// it adds to, while opens on different lanes run concurrently, up to
// MaxConcurrentOpens at once. Each open publishes its own result, so onError
// only sees the failure for the log.
//
// Once ctx is done, opens not yet started are dropped. One already running
// carries on with ctx's values but not its cancellation, so an order is not
// abandoned between persisting and its POST; shutdown waits for it through
// the returned Drainer.
func SubscribeOpen(ctx context.Context, bus Subscriber, execution OpenExecutor, allowed *Allowlist, onError func(error)) (Drainer, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if bus == nil || execution == nil || allowed == nil {
		return nil, fmt.Errorf("NATS subscriber, executor, and strategy allowlist are required")
	}
	dispatcher := newOpenDispatcher(ctx, execution, allowed, onError)
	if err := bus.Subscribe(protocol.SubjectStrategyExecutionOpen, dispatcher.handle); err != nil {
		return nil, err
	}
	return dispatcher, nil
}

type openDispatcher struct {
	*laneWorkers[protocol.ExecutionOpenRequest]
	ctx     context.Context
	exec    OpenExecutor
	allowed *Allowlist
	onError func(error)
}

func newOpenDispatcher(ctx context.Context, exec OpenExecutor, allowed *Allowlist, onError func(error)) *openDispatcher {
	d := &openDispatcher{ctx: ctx, exec: exec, allowed: allowed, onError: onError}
	d.laneWorkers = newLaneWorkers(false, MaxConcurrentOpens, d.stopped, d.execute)
	return d
}

func (d *openDispatcher) handle(_ context.Context, payload []byte) error {
	req, err := natsbus.DecodeJSON[protocol.ExecutionOpenRequest](payload)
	if err != nil {
		return err
	}
	if strings.TrimSpace(req.Strategy) != "" && !d.allowed.Allows(req.Strategy) {
		return nil
	}
	if d.stopped(req) {
		return nil
	}
	d.enqueue(openLaneKey(req), req)
	return nil
}

// openLaneKey names the position an open adds to, as closeLaneKey does.
func openLaneKey(req protocol.ExecutionOpenRequest) string {
	return req.Strategy + "\x00" + req.UniqueTag + "\x00" + req.ConditionID + "\x00" + req.TokenID
}

// stopped drops an open that has not started once shutdown has begun: it
// would only fail partway on the cancelled context.
func (d *openDispatcher) stopped(req protocol.ExecutionOpenRequest) bool {
	if d.ctx.Err() == nil {
		return false
	}
	if d.onError != nil {
		d.onError(fmt.Errorf("dropped open %s for strategy %q: shutting down", req.RequestID, req.Strategy))
	}
	return true
}

func (d *openDispatcher) execute(req protocol.ExecutionOpenRequest) {
	if err := d.exec.ExecuteOpen(context.WithoutCancel(d.ctx), req); err != nil && d.onError != nil {
		d.onError(fmt.Errorf("handle NATS subject %s: %w", protocol.SubjectStrategyExecutionOpen, err))
	}
}
