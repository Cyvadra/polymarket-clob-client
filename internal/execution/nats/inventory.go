package nats

import (
	"context"
	"fmt"
	"strings"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

// InventoryExecutor works split inventory requests (pkg/inventory).
type InventoryExecutor interface {
	ExecuteInventory(context.Context, protocol.ExecutionInventoryRequest) error
}

// SubscribeInventory wires strategy.execution.inventory to the inventory
// manager. As for opens, a request for a strategy outside the allowlist
// belongs to another executiond and is dropped silently. Requests run on one
// worker per lane (unique_tag, condition_id), so a lane's splits and its
// merge apply in the order they arrived; a relayer round trip takes seconds,
// so they never run on the NATS delivery goroutine. Every request publishes
// its own result.
func SubscribeInventory(ctx context.Context, bus Subscriber, inventory InventoryExecutor, allowed *Allowlist, onError func(error)) (Drainer, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if bus == nil || inventory == nil || allowed == nil {
		return nil, fmt.Errorf("NATS subscriber, inventory manager, and strategy allowlist are required")
	}
	d := &inventoryDispatcher{ctx: ctx, exec: inventory, allowed: allowed, onError: onError}
	d.laneWorkers = newLaneWorkers(false, 0, d.stopped, d.execute)
	if err := bus.Subscribe(protocol.SubjectStrategyExecutionInventory, d.handle); err != nil {
		return nil, err
	}
	return d, nil
}

type inventoryDispatcher struct {
	*laneWorkers[protocol.ExecutionInventoryRequest]
	ctx     context.Context
	exec    InventoryExecutor
	allowed *Allowlist
	onError func(error)
}

func (d *inventoryDispatcher) handle(_ context.Context, payload []byte) error {
	req, err := natsbus.DecodeJSON[protocol.ExecutionInventoryRequest](payload)
	if err != nil {
		return err
	}
	if strings.TrimSpace(req.Strategy) != "" && !d.allowed.Allows(req.Strategy) {
		return nil
	}
	if d.stopped(req) {
		return nil
	}
	d.enqueue(laneKey(req.Strategy, req.UniqueTag, req.ConditionID, ""), req)
	return nil
}

func (d *inventoryDispatcher) stopped(req protocol.ExecutionInventoryRequest) bool {
	if d.ctx.Err() == nil {
		return false
	}
	if d.onError != nil {
		d.onError(fmt.Errorf("dropped inventory %s %s for strategy %q: shutting down", req.Action, req.RequestID, req.Strategy))
	}
	return true
}

func (d *inventoryDispatcher) execute(req protocol.ExecutionInventoryRequest, _ func() bool) {
	if err := d.exec.ExecuteInventory(context.WithoutCancel(d.ctx), req); err != nil && d.onError != nil {
		d.onError(fmt.Errorf("handle NATS subject %s: %w", protocol.SubjectStrategyExecutionInventory, err))
	}
}
