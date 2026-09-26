package nats

import (
	"context"
	"fmt"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

// EquitySource values the wallet for the balance query.
type EquitySource interface {
	Snapshot(context.Context) (equity.Snapshot, error)
}

// SubscribeBalanceQuery wires a request/reply handler for
// strategy.execution.balance.query, answering with the wallet's cash and
// equity.
func SubscribeBalanceQuery(bus ReplyConnector, wallet EquitySource) error {
	if bus == nil || wallet == nil {
		return fmt.Errorf("NATS connector and equity source are required")
	}
	return bus.SubscribeReply(protocol.SubjectStrategyExecutionBalanceQuery, func(ctx context.Context, reply string, payload []byte) error {
		if reply == "" {
			return fmt.Errorf("balance query is missing a reply subject")
		}
		response, err := balanceQueryResponse(ctx, wallet, payload)
		if err != nil {
			// Always answer, as the position query does.
			response = protocol.BalanceQueryResponse{SchemaVersion: protocol.SchemaVersionV1, Error: err.Error()}
		}
		if publishErr := bus.PublishJSON(reply, response); publishErr != nil {
			return publishErr
		}
		return err
	})
}

func balanceQueryResponse(ctx context.Context, wallet EquitySource, payload []byte) (protocol.BalanceQueryResponse, error) {
	request, err := natsbus.DecodeJSON[protocol.BalanceQueryRequest](payload)
	if err != nil {
		return protocol.BalanceQueryResponse{}, err
	}
	if request.SchemaVersion != protocol.SchemaVersionV1 {
		return protocol.BalanceQueryResponse{}, fmt.Errorf("balance query requires schema_version %q", protocol.SchemaVersionV1)
	}
	snapshot, err := wallet.Snapshot(ctx)
	if err != nil {
		return protocol.BalanceQueryResponse{}, err
	}
	return protocol.BalanceQueryResponse{
		SchemaVersion:     protocol.SchemaVersionV1,
		CashUSD:           decimal.RoundUSDC(snapshot.CashUSD),
		PositionsValueUSD: decimal.RoundUSDC(snapshot.PositionsUSD),
		EquityUSD:         decimal.RoundUSDC(snapshot.EquityUSD),
		Positions:         snapshot.Positions,
		SettledPositions:  snapshot.SettledPositions,
		UnmarkedPositions: snapshot.UnmarkedPositions,
		CashAsOf:          snapshot.CashAsOf,
		AsOf:              snapshot.AsOf,
	}, nil
}
