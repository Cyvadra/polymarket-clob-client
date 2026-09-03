package executor

import (
	"context"
	"fmt"

	"github.com/Cyvadra/polymarket-clob-client/pkg/contracts"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

type Subscriber interface {
	Subscribe(string, natsbus.Handler) error
}

func SubscribeIntents(bus Subscriber, execution *Executor) error {
	if bus == nil || execution == nil {
		return fmt.Errorf("NATS subscriber and executor are required")
	}
	return bus.Subscribe(contracts.SubjectStrategyExecutionIntent, func(ctx context.Context, payload []byte) error {
		intent, err := natsbus.DecodeJSON[contracts.ExecutionIntent](payload)
		if err != nil {
			return err
		}
		if intent.SchemaVersion != "" && intent.SchemaVersion != contracts.SchemaVersionV1 {
			return fmt.Errorf("unsupported intent schema version %q", intent.SchemaVersion)
		}
		if err := validateIntent(intent); err != nil {
			return err
		}
		return execution.Execute(ctx, intent)
	})
}
