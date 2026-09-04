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

func SubscribeIntents(bus Subscriber, execution *executor.Executor) error {
	if bus == nil || execution == nil {
		return fmt.Errorf("NATS subscriber and executor are required")
	}
	return bus.Subscribe(protocol.SubjectStrategyExecutionIntent, func(ctx context.Context, payload []byte) error {
		intent, err := natsbus.DecodeJSON[protocol.ExecutionIntent](payload)
		if err != nil {
			return err
		}
		return execution.Execute(ctx, intent)
	})
}
