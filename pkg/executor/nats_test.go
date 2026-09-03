package executor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/pkg/contracts"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

type fakeIntentSubscriber struct {
	subject string
	handler natsbus.Handler
}

func (s *fakeIntentSubscriber) Subscribe(subject string, handler natsbus.Handler) error {
	s.subject, s.handler = subject, handler
	return nil
}

func TestSubscribeIntentsDecodesAndExecutes(t *testing.T) {
	execution, err := New(&fakeStore{inserted: true}, &fakeCLOB{response: &clobclient.OrderResponse{OrderID: "order-1"}}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	subscriber := &fakeIntentSubscriber{}
	if err := SubscribeIntents(subscriber, execution); err != nil {
		t.Fatalf("subscribe intents: %v", err)
	}
	if subscriber.subject != contracts.SubjectStrategyExecutionIntent || subscriber.handler == nil {
		t.Fatalf("unexpected subscription: %+v", subscriber)
	}
	payload, err := json.Marshal(testIntent())
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("deliver intent: %v", err)
	}
}

func TestSubscribeIntentsLetsStoreDeduplicateIntent(t *testing.T) {
	repository := &fakeStore{inserted: true}
	client := &fakeCLOB{response: &clobclient.OrderResponse{OrderID: "order-1"}}
	execution, err := New(repository, client, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	subscriber := &fakeIntentSubscriber{}
	if err := SubscribeIntents(subscriber, execution); err != nil {
		t.Fatalf("subscribe intents: %v", err)
	}
	payload, err := json.Marshal(testIntent())
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("deliver first intent: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("deliver duplicate intent: %v", err)
	}
	if client.submits != 1 {
		t.Fatalf("expected duplicate intent to be skipped, submits=%d", client.submits)
	}
}
