package accountfeed

import (
	"context"
	"fmt"

	"github.com/Cyvadra/polymarket-clob-client/pkg/contracts"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

type Subscriber interface {
	Subscribe(string, natsbus.Handler) error
}

func SubscribeFills(bus Subscriber, consumer *FillConsumer) error {
	if bus == nil || consumer == nil {
		return fmt.Errorf("NATS subscriber and fill consumer are required")
	}
	return bus.Subscribe(contracts.SubjectAccountTradeFill, func(ctx context.Context, payload []byte) error {
		fill, err := natsbus.DecodeJSON[contracts.AccountFill](payload)
		if err != nil {
			return err
		}
		if err := validateFill(fill); err != nil {
			return err
		}
		_, err = consumer.Consume(ctx, fill)
		return err
	})
}
