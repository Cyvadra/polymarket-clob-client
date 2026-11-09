package accountfeed

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// ladderStore is a concurrency-safe AggregateOrderStore.
type ladderStore struct {
	mu     sync.Mutex
	intent store.OrderIntentRecord
	orders []store.SignedOrderRecord
	phase  store.LadderPhase
}

func (s *ladderStore) Intent(context.Context, string) (store.OrderIntentRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.intent, nil
}
func (s *ladderStore) IntentOrders(context.Context, string) ([]store.SignedOrderRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.orders), nil
}
func (s *ladderStore) LadderPhase(context.Context, string) (store.LadderPhase, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase, nil
}
func (s *ladderStore) MoveLadderPhase(_ context.Context, _ string, to store.LadderPhase, from ...store.LadderPhase) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(from, s.phase) {
		return false, nil
	}
	s.phase = to
	return true, nil
}
func (s *ladderStore) OrderAveragePrice(context.Context, string) (string, error) {
	return "", store.ErrNotFound
}
func (s *ladderStore) setPhase(phase store.LadderPhase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase = phase
}
func (s *ladderStore) currentPhase() store.LadderPhase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

type failablePublisher struct {
	mu      sync.Mutex
	results []protocol.ExecutionOpenResult
	fail    error
}

func (p *failablePublisher) PublishJSON(subject string, value any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if result, ok := value.(protocol.ExecutionOpenResult); ok && subject == protocol.SubjectExecutionOpenResult {
		if p.fail != nil {
			return p.fail
		}
		p.results = append(p.results, result)
	}
	return nil
}
func (p *failablePublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.results)
}

func newLadderStore(phase store.LadderPhase, states ...statemachine.State) *ladderStore {
	s := &ladderStore{phase: phase, intent: store.OrderIntentRecord{
		IntentID: "i", RequestID: "req", UniqueTag: "lane", Strategy: "s", Kind: store.IntentOpen, ConditionID: "c", TokenID: "t", Outcome: "Up", Side: store.SideBuy,
		Policy: []byte(`{"style":"MULTI_LEVEL_LIMIT"}`),
	}}
	for i, state := range states {
		matched := "0"
		if state == statemachine.StateFilled {
			matched = "10"
		}
		s.orders = append(s.orders, store.SignedOrderRecord{IntentID: "i", ChildSequence: i + 1, State: state, MatchedShares: matched, Price: "0.55", ExchangeOrderID: "x", PostOnly: true, OrderType: store.TimeInForceGTC})
	}
	return s
}

var ladderAt = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func TestPublishLadderResultIsNotDueWhilePlacementRuns(t *testing.T) {
	// Every child so far has ended, but more are still being posted.
	for _, phase := range []store.LadderPhase{store.LadderNone, store.LadderPlacing, store.LadderPublished} {
		s := newLadderStore(phase, statemachine.StateCanceled, statemachine.StateCanceled)
		publisher := &failablePublisher{}
		if err := PublishLadderResult(context.Background(), s, publisher, "i", "r", ladderAt); err != nil {
			t.Fatal(err)
		}
		if publisher.count() != 0 || s.currentPhase() != phase {
			t.Fatalf("phase %q: published=%d phase now %q", phase, publisher.count(), s.currentPhase())
		}
	}
}

func TestPublishLadderResultWaitsForEveryChild(t *testing.T) {
	s := newLadderStore(store.LadderActive, statemachine.StateCanceled, statemachine.StateLive, statemachine.StateFilled)
	publisher := &failablePublisher{}
	if err := PublishLadderResult(context.Background(), s, publisher, "i", "r", ladderAt); err != nil {
		t.Fatal(err)
	}
	if publisher.count() != 0 || s.currentPhase() != store.LadderActive {
		t.Fatalf("published=%d phase=%q with a level still resting", publisher.count(), s.currentPhase())
	}
	if err := PublishLadderResult(context.Background(), newLadderStore(store.LadderActive), publisher, "i", "r", ladderAt); err != nil || publisher.count() != 0 {
		t.Fatalf("an intent with no children published a result (err=%v)", err)
	}
}

func TestPublishLadderResultPublishesOnceWhenObserversRace(t *testing.T) {
	for _, phase := range []store.LadderPhase{store.LadderActive, store.LadderStopping} {
		s := newLadderStore(phase, statemachine.StateFilled, statemachine.StateCanceled, statemachine.StateFilled)
		publisher := &failablePublisher{}
		var wg sync.WaitGroup
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := PublishLadderResult(context.Background(), s, publisher, "i", "r", ladderAt); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if publisher.count() != 1 {
			t.Fatalf("phase %s: %d results, want exactly one", phase, publisher.count())
		}
		if s.currentPhase() != store.LadderPublished {
			t.Fatalf("phase=%s after publishing", s.currentPhase())
		}
		if r := publisher.results[0]; r.Status != protocol.ResultSucceeded || r.FilledShares != 20 || r.AveragePrice != 0.55 {
			t.Fatalf("result=%+v", r)
		}
		// A later observation, such as the reconciler's, adds nothing.
		if err := PublishLadderResult(context.Background(), s, publisher, "i", "r", ladderAt); err != nil || publisher.count() != 1 {
			t.Fatalf("a second observation published again (err=%v, count=%d)", err, publisher.count())
		}
	}
}

func TestPublishLadderResultRestoresThePhaseWhenThePublishFails(t *testing.T) {
	s := newLadderStore(store.LadderStopping, statemachine.StateCanceled)
	publisher := &failablePublisher{fail: errors.New("nats is down")}
	if err := PublishLadderResult(context.Background(), s, publisher, "i", "r", ladderAt); err == nil {
		t.Fatal("a failed publish was not reported")
	}
	if s.currentPhase() != store.LadderStopping {
		t.Fatalf("phase=%s, want the claim undone so the next observer retries", s.currentPhase())
	}
	publisher.mu.Lock()
	publisher.fail = nil
	publisher.mu.Unlock()
	if err := PublishLadderResult(context.Background(), s, publisher, "i", "r", ladderAt); err != nil || publisher.count() != 1 {
		t.Fatalf("retry: err=%v count=%d, want it to publish now", err, publisher.count())
	}
}

func TestPublishLadderResultReportsAnInconsistentLadder(t *testing.T) {
	s := newLadderStore(store.LadderActive, statemachine.StateFilled)
	s.orders[0].MatchedShares = "lots"
	if err := PublishLadderResult(context.Background(), s, &failablePublisher{}, "i", "r", ladderAt); err == nil {
		t.Fatal("garbled matched shares published a result")
	}
	if s.currentPhase() != store.LadderActive {
		t.Fatalf("phase=%s: a result that could not be built must not claim the phase", s.currentPhase())
	}
}

func TestPublishTerminalResultRoutesLadderChildrenToTheAggregate(t *testing.T) {
	s := newLadderStore(store.LadderActive, statemachine.StateFilled)
	publisher := &failablePublisher{}
	order := s.orders[0]
	if err := PublishTerminalResult(publisher, s.intent, order, "r", "0.55", ladderAt); !errors.Is(err, ErrAggregateIntent) {
		t.Fatalf("err=%v, want ErrAggregateIntent so a result is never dropped silently", err)
	}
	if publisher.count() != 0 {
		t.Fatal("a ladder child published a result of its own")
	}
	if err := PublishTerminalResultWithStore(context.Background(), nil, publisher, s.intent, order, "r", "0.55", ladderAt); err == nil {
		t.Fatal("a ladder result with no aggregate store was dropped without an error")
	}
	if err := PublishTerminalResultWithStore(context.Background(), s, publisher, s.intent, order, "r", "0.55", ladderAt); err != nil || publisher.count() != 1 {
		t.Fatalf("err=%v count=%d, want the aggregate published", err, publisher.count())
	}
}

func TestPublishTerminalResultWithStorePassesOrdinaryIntentsThrough(t *testing.T) {
	intent := store.OrderIntentRecord{IntentID: "i", RequestID: "req", UniqueTag: "lane", Kind: store.IntentOpen, Side: store.SideBuy, Policy: []byte(`{"style":"LIMIT"}`)}
	order := store.SignedOrderRecord{IntentID: "i", ChildSequence: store.StrategyChildSequence, State: statemachine.StateFilled, MatchedShares: "3"}
	publisher := &failablePublisher{}
	if err := PublishTerminalResultWithStore(context.Background(), nil, publisher, intent, order, "r", "0.5", ladderAt); err != nil || publisher.count() != 1 {
		t.Fatalf("err=%v count=%d, want an ordinary result without any aggregate store", err, publisher.count())
	}
}
