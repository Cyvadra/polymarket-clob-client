package nats

import (
	"context"
	"fmt"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

// CancelExecutor is the execution surface that consumes cancel commands.
type CancelExecutor interface {
	Cancel(context.Context, protocol.ExecutionCancelRequest) error
}

// SubscribeCancel wires the strategy.execution.cancel subject to the executor.
// Cancels are at-most-once like intents: a decodable request always receives an
// execution.cancel.ack from the executor; malformed JSON can only be logged.
func SubscribeCancel(bus Subscriber, execution CancelExecutor) error {
	if bus == nil || execution == nil {
		return fmt.Errorf("NATS subscriber and cancel executor are required")
	}
	return bus.Subscribe(protocol.SubjectStrategyExecutionCancel, func(ctx context.Context, payload []byte) error {
		request, err := natsbus.DecodeJSON[protocol.ExecutionCancelRequest](payload)
		if err != nil {
			return err
		}
		return execution.Cancel(ctx, request)
	})
}
