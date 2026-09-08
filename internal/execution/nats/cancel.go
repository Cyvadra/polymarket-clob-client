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

// SubscribeClose wires the strategy.execution.close subject to the executor.
func SubscribeClose(bus Subscriber, execution CloseExecutor) error {
	if bus == nil || execution == nil {
		return fmt.Errorf("NATS subscriber and close executor are required")
	}
	return bus.Subscribe(protocol.SubjectStrategyExecutionClose, func(ctx context.Context, payload []byte) error {
		request, err := natsbus.DecodeJSON[protocol.ExecutionCloseRequest](payload)
		if err != nil {
			return err
		}
		return execution.ExecuteClose(ctx, request)
	})
}
