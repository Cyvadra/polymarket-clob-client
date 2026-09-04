package nats

import (
	"context"
	"fmt"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

func SubscribeQuotes(bus Subscriber, cache *marketquotes.Cache) error {
	if bus == nil || cache == nil {
		return fmt.Errorf("NATS subscriber and market quote cache are required")
	}
	return bus.Subscribe(protocol.SubjectMarketQuotes, func(_ context.Context, payload []byte) error {
		snapshot, err := natsbus.DecodeJSON[marketquotes.Snapshot](payload)
		if err != nil {
			return err
		}
		return cache.Put(snapshot)
	})
}
