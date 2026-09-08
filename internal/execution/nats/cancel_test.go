package nats

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type recordedClosePublisher struct {
	values []protocol.ExecutionCloseResult
}

func (p *recordedClosePublisher) PublishJSON(subject string, value any) error {
	if subject != protocol.SubjectExecutionCloseResult {
		return nil
	}
	if result, ok := value.(protocol.ExecutionCloseResult); ok {
		p.values = append(p.values, result)
	}
	return nil
}

func TestSubscribeCloseDecodesAndDispatches(t *testing.T) {
	execution, err := executor.New(fakeStore{positions: []store.PositionRecord{{ConditionID: "condition", TokenID: "token", UniqueTag: "lane-a", Outcome: "Up", PositionSize: "1", ActualShares: "1", AvailableSize: "1", State: "open"}}}, fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	publisher := &recordedClosePublisher{}
	execution.SetEventPublisher(publisher)
	subscriber := &fakeSubscriber{}
	if err := SubscribeClose(subscriber, execution); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if subscriber.subject != protocol.SubjectStrategyExecutionClose || subscriber.handler == nil {
		t.Fatalf("subscription=%+v", subscriber)
	}
	request := protocol.ExecutionCloseRequest{SchemaVersion: protocol.SchemaVersionV1, UniqueTag: "lane-a", Strategy: "strategy", ConditionID: "condition", AssetID: "token", Outcome: "Up", Mode: protocol.ExecutionCloseModeForce}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(publisher.values) != 1 || publisher.values[0].Status != protocol.ResultSucceeded || publisher.values[0].UniqueTag != "lane-a" || publisher.values[0].ConditionID != "condition" || publisher.values[0].AssetID != "token" {
		t.Fatalf("expected close result publication, got %+v", publisher.values)
	}
}

func TestSubscribeCloseRejectsMalformedJSON(t *testing.T) {
	execution, err := executor.New(fakeStore{}, fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	subscriber := &fakeSubscriber{}
	if err := SubscribeClose(subscriber, execution); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := subscriber.handler(context.Background(), []byte(`{not json`)); err == nil {
		t.Fatal("expected malformed JSON to fail")
	}
}
