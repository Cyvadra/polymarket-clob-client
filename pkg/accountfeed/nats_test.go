package accountfeed

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/contracts"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeSubscriber struct {
	subject string
	handler natsbus.Handler
}

func (s *fakeSubscriber) Subscribe(subject string, handler natsbus.Handler) error {
	s.subject, s.handler = subject, handler
	return nil
}

func TestSubscribeFillsDecodesAndConsumes(t *testing.T) {
	repository := &fakeFillStore{}
	consumer, err := NewFillConsumer(repository, time.Now)
	if err != nil {
		t.Fatalf("new fill consumer: %v", err)
	}
	subscriber := &fakeSubscriber{}
	if err := SubscribeFills(subscriber, consumer); err != nil {
		t.Fatalf("subscribe fills: %v", err)
	}
	if subscriber.subject != contracts.SubjectAccountTradeFill || subscriber.handler == nil {
		t.Fatalf("unexpected subscription: %+v", subscriber)
	}
	payload, err := json.Marshal(testFill())
	if err != nil {
		t.Fatalf("marshal fill: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("deliver fill: %v", err)
	}
	if repository.record.FillID != "fill-1" {
		t.Fatalf("fill was not consumed: %+v", repository.record)
	}
}

func TestSubscribeFillsDeduplicatesByFillID(t *testing.T) {
	repository := &fakeFillStore{}
	consumer, err := NewFillConsumer(repository, time.Now)
	if err != nil {
		t.Fatalf("new fill consumer: %v", err)
	}
	subscriber := &fakeSubscriber{}
	if err := SubscribeFills(subscriber, consumer); err != nil {
		t.Fatalf("subscribe fills: %v", err)
	}
	payload, err := json.Marshal(testFill())
	if err != nil {
		t.Fatalf("marshal fill: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("deliver first fill: %v", err)
	}
	if err := subscriber.handler(context.Background(), payload); err != nil {
		t.Fatalf("deliver duplicate fill: %v", err)
	}
	if repository.applied != 1 {
		t.Fatalf("expected one applied fill, got %d", repository.applied)
	}
}

var _ store.FillRepository = (*fakeFillStore)(nil)
