package nats

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor"
)

type recordedCancelPublisher struct {
	acks []protocol.ExecutionCancelAck
}

func (p *recordedCancelPublisher) PublishJSON(subject string, value any) error {
	if subject != protocol.SubjectExecutionCancelAck {
		return nil
	}
	if ack, ok := value.(protocol.ExecutionCancelAck); ok {
		p.acks = append(p.acks, ack)
	}
	return nil
}

func TestSubscribeCancelDecodesAndDispatches(t *testing.T) {
	execution, err := executor.New(fakeStore{}, fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	publisher := &recordedCancelPublisher{}
	execution.SetEventPublisher(publisher)
	subscriber := &fakeSubscriber{}
	if err := SubscribeCancel(subscriber, execution); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if subscriber.subject != protocol.SubjectStrategyExecutionCancel || subscriber.handler == nil {
		t.Fatalf("subscription=%+v", subscriber)
	}
	request := protocol.ExecutionCancelRequest{SchemaVersion: protocol.SchemaVersionV1, IntentID: "intent"}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(publisher.acks) != 1 || publisher.acks[0].IntentID != "intent" || publisher.acks[0].Status != protocol.CancelNotFound {
		t.Fatalf("expected unknown intent to be acknowledged, got %+v", publisher.acks)
	}
}

func TestSubscribeCancelRejectsMalformedJSON(t *testing.T) {
	execution, err := executor.New(fakeStore{}, fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	subscriber := &fakeSubscriber{}
	if err := SubscribeCancel(subscriber, execution); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := subscriber.handler(context.Background(), []byte(`{not json`)); err == nil {
		t.Fatal("expected malformed JSON to fail")
	}
}
