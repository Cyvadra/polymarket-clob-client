package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/executor"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
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

type fakeStore struct{ positions []store.PositionRecord }

func (fakeStore) WithIntentLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}
func (fakeStore) InsertIntent(context.Context, store.OrderIntentRecord) (bool, error) {
	return true, nil
}
func (fakeStore) Intent(_ context.Context, intentID string) (store.OrderIntentRecord, error) {
	return store.OrderIntentRecord{IntentID: intentID, UniqueTag: "lane-a", Kind: store.IntentOpen, ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: store.SideBuy}, nil
}
func (fakeStore) UpdateIntentStatus(context.Context, string, string) error { return nil }
func (fakeStore) MarkIntentCanceling(context.Context, string, int, string) (bool, error) {
	return false, nil
}
func (fakeStore) PersistSubmittingOrder(_ context.Context, record store.SignedOrderRecord, _ string) (store.SignedOrderRecord, error) {
	record.State, record.Revision = statemachine.StateSubmitting, record.Revision+1
	return record, nil
}
func (fakeStore) PersistSignedOrder(context.Context, store.SignedOrderRecord) error { return nil }
func (fakeStore) TransitionOrder(_ context.Context, order store.SignedOrderRecord, event statemachine.Event, matched, exchangeID, _ string) (store.SignedOrderRecord, error) {
	transition, _, err := statemachine.Apply(order.State, event)
	if err != nil {
		return store.SignedOrderRecord{}, err
	}
	order.State, order.MatchedShares, order.ExchangeOrderID, order.Revision = transition.To, matched, exchangeID, order.Revision+1
	return order, nil
}
func (fakeStore) OrderByExchangeID(context.Context, string) (store.SignedOrderRecord, error) {
	return store.SignedOrderRecord{}, store.ErrNotFound
}
func (fakeStore) OrderByIntent(context.Context, string, int) (store.SignedOrderRecord, error) {
	return store.SignedOrderRecord{}, store.ErrNotFound
}
func (fakeStore) OpenOrders(context.Context) ([]store.SignedOrderRecord, error) { return nil, nil }
func (fakeStore) Reserve(context.Context, store.ReservationRecord) error        { return nil }
func (fakeStore) Reservation(context.Context, string) (store.ReservationRecord, error) {
	return store.ReservationRecord{}, store.ErrNotFound
}
func (fakeStore) Release(context.Context, string, string) error             { return nil }
func (fakeStore) ApplyFill(context.Context, store.FillRecord) (bool, error) { return false, nil }
func (s fakeStore) LatestLimitCloseIntent(context.Context, string, string, string) (store.OrderIntentRecord, error) {
	return store.OrderIntentRecord{}, store.ErrNotFound
}
func (s fakeStore) ReconcilePositionSize(context.Context, string, string, string, string) error {
	return nil
}
func (s fakeStore) PositionFeatures(context.Context) ([]store.PositionRecord, error) {
	return s.positions, nil
}
func (s fakeStore) LanePosition(_ context.Context, conditionID, tokenID, uniqueTag string) (store.PositionRecord, bool, error) {
	for _, p := range s.positions {
		if p.ConditionID == conditionID && p.TokenID == tokenID && p.UniqueTag == uniqueTag {
			return p, true, nil
		}
	}
	return store.PositionRecord{}, false, nil
}

// fakeCLOB satisfies the executor CLOB interface.
type fakeCLOB struct{}

func (fakeCLOB) CreateOrder(context.Context, clobclient.UserOrder) (clobclient.SignedOrderV2, error) {
	return clobclient.SignedOrderV2{Salt: 1}, nil
}
func (fakeCLOB) SubmitSignedOrder(context.Context, clobclient.SignedOrderV2, clobclient.OrderType, bool) (*clobclient.OrderResponse, error) {
	return &clobclient.OrderResponse{OrderID: "order-1"}, nil
}
func (fakeCLOB) CancelOrder(context.Context, string) error             { return nil }
func (fakeCLOB) TickSize(context.Context, string) (float64, error)     { return 0.01, nil }
func (fakeCLOB) MinOrderSize(context.Context, string) (float64, error) { return 0, nil }

type recordedPublisher struct {
	subject string
	value   any
}

func (p *recordedPublisher) PublishJSON(subject string, value any) error {
	p.subject = subject
	p.value = value
	return nil
}

// subscribeOpen subscribes through SubscribeOpen and returns the handler it
// registered, the Drainer it returned, and the errors reported to onError.
func subscribeOpen(t *testing.T, exec OpenExecutor, allowed *Allowlist) (natsbus.Handler, Drainer, *[]error) {
	t.Helper()
	var mu sync.Mutex
	failures := &[]error{}
	subscriber := &fakeSubscriber{}
	work, err := SubscribeOpen(context.Background(), subscriber, exec, allowed, func(err error) {
		mu.Lock()
		defer mu.Unlock()
		*failures = append(*failures, err)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if subscriber.subject != protocol.SubjectStrategyExecutionOpen || subscriber.handler == nil {
		t.Fatalf("subscription=%+v", subscriber)
	}
	return subscriber.handler, work, failures
}

func deliverOpen(t *testing.T, handler natsbus.Handler, req protocol.ExecutionOpenRequest) {
	t.Helper()
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := handler(context.Background(), payload); err != nil {
		t.Fatalf("deliver: %v", err)
	}
}

func drain(t *testing.T, work Drainer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := work.Wait(ctx); err != nil {
		t.Fatalf("wait for opens: %v", err)
	}
}

func TestSubscribeOpenDecodesAndExecutes(t *testing.T) {
	execution, err := executor.New(fakeStore{}, fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	handler, work, failures := subscribeOpen(t, execution, testAllowlist(t))
	deliverOpen(t, handler, protocol.ExecutionOpenRequest{SchemaVersion: protocol.SchemaVersionV1, UniqueTag: "lane-a", Strategy: "strategy", ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: protocol.SideBuy, TargetUSD: "1", LimitPrice: "0.5", TimeInForce: protocol.TimeInForceGTC, Policy: protocol.ExecutionPolicy{CompleteWithinMillis: 1, Style: protocol.ExecutionStyleLimit}})
	drain(t, work)
	if len(*failures) != 0 {
		t.Fatalf("execute: %v", *failures)
	}
}

func TestSubscribeOpenPublishesFailureForInvalidIntent(t *testing.T) {
	execution, err := executor.New(fakeStore{}, fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	publisher := &recordedPublisher{}
	execution.SetEventPublisher(publisher)
	handler, work, failures := subscribeOpen(t, execution, testAllowlist(t))
	deliverOpen(t, handler, protocol.ExecutionOpenRequest{SchemaVersion: protocol.SchemaVersionV1, UniqueTag: "lane-a", Strategy: "strategy", ConditionID: "condition", TokenID: "token", Outcome: "Up", Side: protocol.SideBuy, TargetUSD: "1", LimitPrice: "0.5", TimeInForce: protocol.TimeInForceGTC, Policy: protocol.ExecutionPolicy{CompleteWithinMillis: 1, Style: "UNSUPPORTED"}})
	drain(t, work)
	if len(*failures) != 1 {
		t.Fatalf("expected one invalid intent error, got %v", *failures)
	}
	result, ok := publisher.value.(protocol.ExecutionOpenResult)
	if !ok || publisher.subject != protocol.SubjectExecutionOpenResult || result.UniqueTag != "lane-a" || result.ConditionID != "condition" || result.TokenID != "token" || result.Status != protocol.ResultFailed || result.ReasonCode != "UNSUPPORTED_EXECUTION_STYLE" {
		t.Fatalf("result=%+v subject=%q", publisher.value, publisher.subject)
	}
}

// gatedOpenExecutor holds every open until release is closed. It records the
// order opens entered and the most that were inside ExecuteOpen at once.
type gatedOpenExecutor struct {
	entered chan protocol.ExecutionOpenRequest
	release chan struct{}
	ctxErr  chan error

	mu      sync.Mutex
	inside  int
	maxSeen int
	order   []string
}

func newGatedOpenExecutor() *gatedOpenExecutor {
	return &gatedOpenExecutor{entered: make(chan protocol.ExecutionOpenRequest, 16), release: make(chan struct{}), ctxErr: make(chan error, 16)}
}

func (g *gatedOpenExecutor) ExecuteOpen(ctx context.Context, req protocol.ExecutionOpenRequest) error {
	g.mu.Lock()
	g.inside++
	g.maxSeen = max(g.maxSeen, g.inside)
	g.order = append(g.order, req.RequestID)
	g.mu.Unlock()
	g.entered <- req
	<-g.release
	g.ctxErr <- ctx.Err()
	g.mu.Lock()
	g.inside--
	g.mu.Unlock()
	return nil
}

func openOn(strategy, tag, requestID string) protocol.ExecutionOpenRequest {
	return protocol.ExecutionOpenRequest{RequestID: requestID, Strategy: strategy, UniqueTag: tag, ConditionID: "condition", TokenID: "token"}
}

func awaitEntered(t *testing.T, g *gatedOpenExecutor, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-g.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d opens started", i, n)
		}
	}
}

// Opens from different strategies must not wait out each other's exchange
// round trip.
func TestSubscribeOpenRunsLanesConcurrently(t *testing.T) {
	allowed, err := ParseAllowlist("alpha,beta")
	if err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	g := newGatedOpenExecutor()
	handler, work, _ := subscribeOpen(t, g, allowed)
	deliverOpen(t, handler, openOn("alpha", "lane-a", "1"))
	deliverOpen(t, handler, openOn("beta", "lane-a", "2"))
	awaitEntered(t, g, 2)
	close(g.release)
	drain(t, work)
}

// However many lanes have opens, no more than MaxConcurrentOpens run at once.
func TestSubscribeOpenBoundsConcurrency(t *testing.T) {
	g := newGatedOpenExecutor()
	handler, work, _ := subscribeOpen(t, g, testAllowlist(t))
	for i := 0; i < MaxConcurrentOpens+2; i++ {
		deliverOpen(t, handler, openOn("strategy", fmt.Sprintf("lane-%d", i), fmt.Sprint(i)))
	}
	awaitEntered(t, g, MaxConcurrentOpens)
	select {
	case req := <-g.entered:
		t.Fatalf("open %s started beyond the limit of %d", req.RequestID, MaxConcurrentOpens)
	case <-time.After(50 * time.Millisecond):
	}
	close(g.release)
	drain(t, work)
	if g.maxSeen != MaxConcurrentOpens {
		t.Fatalf("at most %d opens ran at once, want %d", g.maxSeen, MaxConcurrentOpens)
	}
}

// A top-up must not overtake the entry it adds to.
func TestSubscribeOpenKeepsALanesOpensInOrder(t *testing.T) {
	g := newGatedOpenExecutor()
	handler, work, _ := subscribeOpen(t, g, testAllowlist(t))
	for _, id := range []string{"1", "2", "3"} {
		deliverOpen(t, handler, openOn("strategy", "lane-a", id))
	}
	awaitEntered(t, g, 1)
	select {
	case req := <-g.entered:
		t.Fatalf("open %s started while an earlier open on its lane was running", req.RequestID)
	case <-time.After(50 * time.Millisecond):
	}
	close(g.release)
	drain(t, work)
	if got := strings.Join(g.order, ","); got != "1,2,3" {
		t.Fatalf("lane ran opens in order %s, want 1,2,3", got)
	}
}

// At shutdown an open already running finishes uncancelled, and opens not
// yet started are dropped.
func TestOpenDispatcherAtShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := newGatedOpenExecutor()
	var dropped int
	d := newOpenDispatcher(ctx, g, testAllowlist(t), func(error) { dropped++ })
	for _, id := range []string{"1", "2"} {
		payload, _ := json.Marshal(openOn("strategy", "lane-a", id))
		if err := d.handle(context.Background(), payload); err != nil {
			t.Fatalf("deliver: %v", err)
		}
	}
	awaitEntered(t, g, 1)
	cancel()
	payload, _ := json.Marshal(openOn("strategy", "lane-b", "3"))
	if err := d.handle(context.Background(), payload); err != nil {
		t.Fatalf("deliver after shutdown: %v", err)
	}
	close(g.release)
	drain(t, d)
	if err := <-g.ctxErr; err != nil {
		t.Fatalf("running open saw a cancelled context: %v", err)
	}
	if got := strings.Join(g.order, ","); got != "1" || dropped != 2 {
		t.Fatalf("ran %q and dropped %d, want only 1 run and 2 dropped", got, dropped)
	}
}
