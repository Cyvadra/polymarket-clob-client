// Package nats contains NATS adapters for executiond's internal handlers.
package nats

import (
	"context"
	"fmt"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

type Subscriber interface {
	Subscribe(string, natsbus.Handler) error
}

func SubscribeOpen(bus Subscriber, execution *executor.Executor) error {
	if bus == nil || execution == nil {
		return fmt.Errorf("NATS subscriber and executor are required")
	}
	return bus.Subscribe(protocol.SubjectStrategyExecutionOpen, func(ctx context.Context, payload []byte) error {
		req, err := natsbus.DecodeJSON[protocol.ExecutionOpenRequest](payload)
		if err != nil {
			return err
		}
		return execution.ExecuteOpen(ctx, req)
	})
}
