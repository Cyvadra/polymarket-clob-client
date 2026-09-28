// Package nats contains NATS adapters for executiond's internal handlers.
package nats

import (
	"context"
	"fmt"
	"strings"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

type Subscriber interface {
	Subscribe(string, natsbus.Handler) error
}

// SubscribeOpen wires the strategy.execution.open subject to the executor.
// An open for a strategy outside the allowlist belongs to another executiond
// on the bus. It is dropped without a result or a log line: a failure result
// would read to the strategy as the owning wallet refusing the order. An open
// with no strategy belongs to no wallet, so every instance answers it with
// the validation failure.
func SubscribeOpen(bus Subscriber, execution *executor.Executor, allowed *Allowlist) error {
	if bus == nil || execution == nil || allowed == nil {
		return fmt.Errorf("NATS subscriber, executor, and strategy allowlist are required")
	}
	return bus.Subscribe(protocol.SubjectStrategyExecutionOpen, func(ctx context.Context, payload []byte) error {
		req, err := natsbus.DecodeJSON[protocol.ExecutionOpenRequest](payload)
		if err != nil {
			return err
		}
		if strings.TrimSpace(req.Strategy) != "" && !allowed.Allows(req.Strategy) {
			return nil
		}
		return execution.ExecuteOpen(ctx, req)
	})
}
