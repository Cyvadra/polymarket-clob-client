package nats

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeReplyConnector struct {
	subject  string
	handler  natsbus.ReplyHandler
	replyTo  string
	response protocol.PositionQueryResponse
}

func (c *fakeReplyConnector) SubscribeReply(subject string, handler natsbus.ReplyHandler) error {
	c.subject, c.handler = subject, handler
	return nil
}

func (c *fakeReplyConnector) PublishJSON(subject string, value any) error {
	c.replyTo = subject
	if response, ok := value.(protocol.PositionQueryResponse); ok {
		c.response = response
	}
	return nil
}

type fakePositionStore struct {
	records []store.PositionRecord
}

func (f fakePositionStore) PositionFeatures(context.Context) ([]store.PositionRecord, error) {
	return f.records, nil
}

func queryPositions() []store.PositionRecord {
	return []store.PositionRecord{
		{MarketID: "market-a", ConditionID: "condition-a", TokenID: "token-a", UniqueTag: "lane-a", Outcome: "Up", PositionSize: "3", AvailableSize: "2", ReservedSize: "1", EntryPrice: "0.42", State: "open"},
		{MarketID: "market-a", ConditionID: "condition-a", TokenID: "token-a", UniqueTag: "lane-b", Outcome: "Up", PositionSize: "4", AvailableSize: "4", ReservedSize: "0", EntryPrice: "0.43", State: "open"},
		{MarketID: "market-b", ConditionID: "condition-b", TokenID: "token-b", UniqueTag: "lane-c", Outcome: "Down", PositionSize: "5", AvailableSize: "5", ReservedSize: "0", EntryPrice: "0.55", State: "open"},
		{MarketID: "market-a", ConditionID: "condition-a", TokenID: "token-empty", UniqueTag: "lane-a", Outcome: "Up", PositionSize: "0", AvailableSize: "0", ReservedSize: "0", State: "empty"},
	}
}

func deliverPositionQuery(t *testing.T, connector *fakeReplyConnector, request protocol.PositionQueryRequest) error {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal query: %v", err)
	}
	return connector.handler(context.Background(), "reply.subject", payload)
}

func TestSubscribePositionQuerySubscribesAndReturnsAllPositions(t *testing.T) {
	connector := &fakeReplyConnector{}
	positions := fakePositionStore{records: queryPositions()}
	if err := SubscribePositionQuery(connector, positions, time.Now); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if connector.subject != protocol.SubjectStrategyExecutionPositionQuery || connector.handler == nil {
		t.Fatalf("subscription=%+v", connector)
	}
	if err := deliverPositionQuery(t, connector, protocol.PositionQueryRequest{SchemaVersion: protocol.SchemaVersionV1}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if connector.replyTo != "reply.subject" {
		t.Fatalf("expected reply on request subject, got %q", connector.replyTo)
	}
	if len(connector.response.Positions) != 3 {
		t.Fatalf("expected three open positions, got %d", len(connector.response.Positions))
	}
	for _, feature := range connector.response.Positions {
		if feature.PositionSize == "0" || !feature.HasPosition {
			t.Fatalf("empty position leaked into response: %+v", feature)
		}
	}
}

func TestSubscribePositionQueryFiltersByCondition(t *testing.T) {
	connector := &fakeReplyConnector{}
	positions := fakePositionStore{records: queryPositions()}
	if err := SubscribePositionQuery(connector, positions, time.Now); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	request := protocol.PositionQueryRequest{SchemaVersion: protocol.SchemaVersionV1, ConditionID: "condition-a"}
	if err := deliverPositionQuery(t, connector, request); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(connector.response.Positions) != 2 || connector.response.Positions[0].ConditionID != "condition-a" || connector.response.Positions[1].ConditionID != "condition-a" {
		t.Fatalf("expected condition-a only, got %+v", connector.response.Positions)
	}
}

func TestSubscribePositionQueryFiltersByUniqueTag(t *testing.T) {
	connector := &fakeReplyConnector{}
	positions := fakePositionStore{records: queryPositions()}
	if err := SubscribePositionQuery(connector, positions, time.Now); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	request := protocol.PositionQueryRequest{SchemaVersion: protocol.SchemaVersionV1, ConditionID: "condition-a", UniqueTag: "lane-b"}
	if err := deliverPositionQuery(t, connector, request); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(connector.response.Positions) != 1 || connector.response.Positions[0].UniqueTag != "lane-b" {
		t.Fatalf("expected lane-b only, got %+v", connector.response.Positions)
	}
}

func TestSubscribePositionQueryFiltersByMarket(t *testing.T) {
	connector := &fakeReplyConnector{}
	positions := fakePositionStore{records: queryPositions()}
	if err := SubscribePositionQuery(connector, positions, time.Now); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	request := protocol.PositionQueryRequest{SchemaVersion: protocol.SchemaVersionV1, MarketID: "market-b"}
	if err := deliverPositionQuery(t, connector, request); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(connector.response.Positions) != 1 || connector.response.Positions[0].MarketID != "market-b" {
		t.Fatalf("expected market-b only, got %+v", connector.response.Positions)
	}
}

func TestSubscribePositionQueryRejectsInvalidRequest(t *testing.T) {
	connector := &fakeReplyConnector{}
	positions := fakePositionStore{records: queryPositions()}
	if err := SubscribePositionQuery(connector, positions, time.Now); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := deliverPositionQuery(t, connector, protocol.PositionQueryRequest{}); err == nil {
		t.Fatal("expected invalid schema to fail")
	}
	if err := connector.handler(context.Background(), "reply.subject", []byte(`{not json`)); err == nil {
		t.Fatal("expected malformed JSON to fail")
	}
	if err := connector.handler(context.Background(), "", []byte(`{}`)); err == nil {
		t.Fatal("expected missing reply subject to fail")
	}
}

// A requester that gets no reply can only time out, and the cause would be
// visible in this daemon's log alone.
func TestPositionQueryRepliesWithAnErrorInsteadOfStayingSilent(t *testing.T) {
	connector := &fakeReplyConnector{}
	if err := SubscribePositionQuery(connector, fakePositionStore{records: queryPositions()}, time.Now); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := deliverPositionQuery(t, connector, protocol.PositionQueryRequest{}); err == nil {
		t.Fatal("expected a schema version rejection")
	}
	if connector.replyTo != "reply.subject" || connector.response.Error == "" {
		t.Fatalf("expected an error reply on the request subject, got %+v", connector.response)
	}
}

func TestPositionQueryRepliesWithAnErrorOnUndecodablePayload(t *testing.T) {
	connector := &fakeReplyConnector{}
	if err := SubscribePositionQuery(connector, fakePositionStore{}, time.Now); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := connector.handler(context.Background(), "reply.subject", []byte("not json")); err == nil {
		t.Fatal("expected a decode failure")
	}
	if connector.response.Error == "" {
		t.Fatalf("expected an error reply, got %+v", connector.response)
	}
}
