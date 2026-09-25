package nats

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/equity"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

type fakeBalanceConnector struct {
	subject  string
	handler  natsbus.ReplyHandler
	response protocol.BalanceQueryResponse
}

func (c *fakeBalanceConnector) SubscribeReply(subject string, handler natsbus.ReplyHandler) error {
	c.subject, c.handler = subject, handler
	return nil
}

func (c *fakeBalanceConnector) PublishJSON(_ string, value any) error {
	c.response = value.(protocol.BalanceQueryResponse)
	return nil
}

type fakeEquity struct {
	snapshot equity.Snapshot
	err      error
}

func (f fakeEquity) Snapshot(context.Context) (equity.Snapshot, error) { return f.snapshot, f.err }

func deliverBalanceQuery(t *testing.T, connector *fakeBalanceConnector, schema string) error {
	t.Helper()
	payload, _ := json.Marshal(protocol.BalanceQueryRequest{SchemaVersion: schema})
	return connector.handler(context.Background(), "reply.subject", payload)
}

func TestBalanceQueryReturnsEquity(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	connector := &fakeBalanceConnector{}
	wallet := fakeEquity{snapshot: equity.Snapshot{CashUSD: 100.5, PositionsUSD: 0.1 + 0.2, EquityUSD: 100.8, Positions: 2, UnmarkedPositions: 1, CashAsOf: at, AsOf: at}}
	if err := SubscribeBalanceQuery(connector, wallet); err != nil {
		t.Fatal(err)
	}
	if connector.subject != protocol.SubjectStrategyExecutionBalanceQuery {
		t.Fatalf("subject=%q", connector.subject)
	}
	if err := deliverBalanceQuery(t, connector, protocol.SchemaVersionV1); err != nil {
		t.Fatal(err)
	}
	got := connector.response
	if got.CashUSD != 100.5 || got.PositionsValueUSD != 0.3 || got.EquityUSD != 100.8 || got.Positions != 2 || got.UnmarkedPositions != 1 || got.Error != "" {
		t.Fatalf("response=%+v", got)
	}
}

func TestBalanceQueryAnswersWithError(t *testing.T) {
	connector := &fakeBalanceConnector{}
	if err := SubscribeBalanceQuery(connector, fakeEquity{err: errors.New("exchange down")}); err != nil {
		t.Fatal(err)
	}
	if err := deliverBalanceQuery(t, connector, protocol.SchemaVersionV1); err == nil || connector.response.Error == "" {
		t.Fatalf("err=%v response=%+v", err, connector.response)
	}
	if err := deliverBalanceQuery(t, connector, "execution.v0"); err == nil || connector.response.Error == "" {
		t.Fatalf("schema mismatch not reported: %+v", connector.response)
	}
}
