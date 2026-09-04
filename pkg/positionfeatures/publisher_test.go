package positionfeatures

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type fakeStore struct {
	positions []store.PositionRecord
	err       error
}

func (s fakeStore) Position(context.Context, string, string) (store.PositionRecord, error) {
	return store.PositionRecord{}, store.ErrNotFound
}

func (s fakeStore) PositionFeatures(context.Context) ([]store.PositionRecord, error) {
	return s.positions, s.err
}

type publishedMessage struct {
	subject string
	value   protocol.PositionFeature
}

type fakePublisher struct {
	messages []publishedMessage
	err      error
}

func (p *fakePublisher) PublishJSON(subject string, value any) error {
	if p.err != nil {
		return p.err
	}
	feature, ok := value.(protocol.PositionFeature)
	if !ok {
		return errors.New("unexpected payload type")
	}
	p.messages = append(p.messages, publishedMessage{subject: subject, value: feature})
	return nil
}

func TestPublishBuildsStrategyPositionFeatures(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	entry := now.Add(-3 * time.Minute)
	publisher := &fakePublisher{}
	module, err := New(fakeStore{positions: []store.PositionRecord{
		{ConditionID: "condition-1", TokenID: "token-up", Outcome: "Up", PositionSize: "12.5", AvailableSize: "10", ReservedSize: "2.5", EntryPrice: "0.42", EntryTime: entry, State: "open", SourceRevision: 8, UpdatedAt: now.Add(-time.Second)},
		{ConditionID: "condition-1", TokenID: "token-down", Outcome: "Down", PositionSize: "0", AvailableSize: "0", ReservedSize: "0", State: "empty", SourceRevision: 9, UpdatedAt: now.Add(-time.Second)},
	}}, publisher, func() time.Time { return now }, 0)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := module.Publish(context.Background()); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if len(publisher.messages) != 2 {
		t.Fatalf("published messages = %d, want 2", len(publisher.messages))
	}
	open := publisher.messages[0]
	if open.subject != "position.features.condition-1.token-up" {
		t.Errorf("open subject = %q", open.subject)
	}
	if !open.value.HasPosition || open.value.EntryPrice == nil || *open.value.EntryPrice != "0.42" {
		t.Errorf("open feature position fields = %+v", open.value)
	}
	if open.value.EntryTime == nil || !open.value.EntryTime.Equal(entry) || open.value.SecondsSinceEntry != 180 {
		t.Errorf("open feature timing fields = %+v", open.value)
	}
	if open.value.Seq != 1 || open.value.SchemaVersion != protocol.SchemaVersionV1 {
		t.Errorf("open feature metadata = %+v", open.value)
	}

	empty := publisher.messages[1]
	if empty.subject != "position.features.condition-1.token-down" {
		t.Errorf("empty subject = %q", empty.subject)
	}
	if empty.value.HasPosition || empty.value.EntryPrice != nil || empty.value.EntryTime != nil || empty.value.SecondsSinceEntry != 0 {
		t.Errorf("empty feature position fields = %+v", empty.value)
	}
	if empty.value.Seq != 2 {
		t.Errorf("empty feature sequence = %d, want 2", empty.value.Seq)
	}
}

func TestPublishPropagatesStoreAndBusErrors(t *testing.T) {
	storeErr := errors.New("store unavailable")
	module, err := New(fakeStore{err: storeErr}, &fakePublisher{}, time.Now, time.Second)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := module.Publish(context.Background()); !errors.Is(err, storeErr) {
		t.Fatalf("Publish() error = %v, want store error", err)
	}

	busErr := errors.New("NATS unavailable")
	module, err = New(fakeStore{positions: []store.PositionRecord{{ConditionID: "condition", TokenID: "token"}}}, &fakePublisher{err: busErr}, time.Now, time.Second)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := module.Publish(context.Background()); !errors.Is(err, busErr) {
		t.Fatalf("Publish() error = %v, want bus error", err)
	}
}

func TestPublishSkipsUnchangedPosition(t *testing.T) {
	now := time.Now
	publisher := &fakePublisher{}
	module, err := New(fakeStore{positions: []store.PositionRecord{{ConditionID: "condition", TokenID: "token", SourceRevision: 4}}}, publisher, now, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := module.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := module.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(publisher.messages) != 1 {
		t.Fatalf("published messages = %d, want 1", len(publisher.messages))
	}
}

func TestRunContinuesAfterPublishError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publisher := &fakePublisher{err: errors.New("NATS unavailable")}
	module, err := New(fakeStore{positions: []store.PositionRecord{{ConditionID: "condition", TokenID: "token"}}}, publisher, time.Now, time.Hour)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	errorsSeen := 0
	module.SetErrorHandler(func(error) {
		errorsSeen++
		cancel()
	})
	if err := module.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if errorsSeen != 1 {
		t.Fatalf("expected one handled publish error, got %d", errorsSeen)
	}
}
