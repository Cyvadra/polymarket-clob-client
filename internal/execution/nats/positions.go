package nats

import (
	"context"
	"fmt"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// ReplyConnector is the transport surface the position query needs: it must be
// able to receive a request together with its reply subject and to publish the
// response back on that subject.
type ReplyConnector interface {
	SubscribeReply(string, natsbus.ReplyHandler) error
	PublishJSON(string, any) error
}

// PositionStore is the durable source for the position query.
type PositionStore interface {
	PositionFeatures(context.Context) ([]store.PositionRecord, error)
}

// SubscribePositionQuery wires a request/reply handler for
// strategy.execution.position.query. A request without a market or condition
// filter returns every currently held position; otherwise the reply is
// filtered to the requested market. Responses reuse the strategy-facing
// PositionFeature shape.
func SubscribePositionQuery(bus ReplyConnector, positions PositionStore, now func() time.Time) error {
	if bus == nil || positions == nil {
		return fmt.Errorf("NATS connector and position store are required")
	}
	if now == nil {
		now = time.Now
	}
	return bus.SubscribeReply(protocol.SubjectStrategyExecutionPositionQuery, func(ctx context.Context, reply string, payload []byte) error {
		if reply == "" {
			return fmt.Errorf("position query is missing a reply subject")
		}
		request, err := natsbus.DecodeJSON[protocol.PositionQueryRequest](payload)
		if err != nil {
			return err
		}
		if request.SchemaVersion != protocol.SchemaVersionV1 {
			return fmt.Errorf("position query requires schema_version %q", protocol.SchemaVersionV1)
		}
		records, err := positions.PositionFeatures(ctx)
		if err != nil {
			return fmt.Errorf("load position features for query: %w", err)
		}
		publishedAt := now().UTC()
		response := protocol.PositionQueryResponse{SchemaVersion: protocol.SchemaVersionV1, Positions: make([]protocol.PositionFeature, 0)}
		for _, record := range records {
			if request.ConditionID != "" && record.ConditionID != request.ConditionID {
				continue
			}
			if request.MarketID != "" && record.MarketID != request.MarketID {
				continue
			}
			// Empty rows (no current holding) are not positions.
			if !decimal.Positive(record.PositionSize) {
				continue
			}
			response.Positions = append(response.Positions, store.BuildPositionFeature(record, 0, publishedAt))
		}
		return bus.PublishJSON(reply, response)
	})
}
