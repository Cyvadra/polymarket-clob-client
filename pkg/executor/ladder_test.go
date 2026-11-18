package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/mapping"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// memStore is a concurrency-safe store for ladder tests. It embeds fakeStore
// for positions and reservations and replaces everything about intents and
// orders, which a ladder needs many of at once.
type memStore struct {
	*fakeStore
	mu      sync.Mutex
	intents map[string]store.OrderIntentRecord
	orders  map[orderKey]store.SignedOrderRecord
	phases  map[string]store.LadderPhase
	// buyBudget, when positive, is how many BUY reservations succeed before
	// the rest are refused for the exposure limit.
	buyBudget int
	// onPhase, when set, runs after each ladder phase move, outside mu.
	onPhase func(id string, to store.LadderPhase)
}

func (s *memStore) Reserve(ctx context.Context, record store.ReservationRecord) error {
	s.mu.Lock()
	if record.Side == "BUY" && s.buyBudget != 0 {
		if s.buyBudget < 0 {
			s.mu.Unlock()
			return store.ErrExposureLimit
		}
		if s.buyBudget--; s.buyBudget == 0 {
			s.buyBudget = -1
		}
	}
	s.mu.Unlock()
	return s.fakeStore.Reserve(ctx, record)
}

func newMemStore() *memStore {
	return &memStore{fakeStore: &fakeStore{inserted: true}, intents: map[string]store.OrderIntentRecord{}, orders: map[orderKey]store.SignedOrderRecord{}, phases: map[string]store.LadderPhase{}}
}

func (s *memStore) InsertIntent(_ context.Context, record store.OrderIntentRecord) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intents[record.IntentID] = record
	return true, nil
}

func (s *memStore) Intent(_ context.Context, id string) (store.OrderIntentRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if intent, ok := s.intents[id]; ok {
		return intent, nil
	}
	return store.OrderIntentRecord{}, store.ErrNotFound
}

func (s *memStore) UpdateIntentStatus(_ context.Context, id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	intent, ok := s.intents[id]
	if !ok {
		return store.ErrNotFound
	}
	intent.Status = statemachine.State(status)
	s.intents[id] = intent
	return nil
}

func (s *memStore) MarkIntentCanceling(_ context.Context, id string, sequence int, status string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order := s.orders[orderKey{id, sequence}]
	if order.State != statemachine.StateLive && order.State != statemachine.StatePartiallyFilled {
		return false, nil
	}
	intent := s.intents[id]
	intent.Status = statemachine.State(status)
	s.intents[id] = intent
	return true, nil
}

func (s *memStore) PersistSignedOrder(_ context.Context, record store.SignedOrderRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[orderKey{record.IntentID, record.ChildSequence}] = record
	return nil
}

func (s *memStore) PersistSubmittingOrder(ctx context.Context, record store.SignedOrderRecord, reason string) (store.SignedOrderRecord, error) {
	if err := s.PersistSignedOrder(ctx, record); err != nil {
		return store.SignedOrderRecord{}, err
	}
	return s.TransitionOrder(ctx, record, statemachine.EventSubmitStarted, "0", "", reason)
}

// move applies one event under the lock, with the store's revision guard and
// its rule that a drift or deadline marker survives every state but FILLED.
func (s *memStore) move(order store.SignedOrderRecord, event statemachine.Event, matched, exchangeID string) (store.SignedOrderRecord, error) {
	key := orderKey{order.IntentID, order.ChildSequence}
	stored, ok := s.orders[key]
	if !ok {
		return store.SignedOrderRecord{}, store.ErrNotFound
	}
	if stored.Revision != order.Revision || stored.State != order.State {
		return store.SignedOrderRecord{}, store.ErrConflict
	}
	transition, _, err := statemachine.Apply(stored.State, event)
	if err != nil {
		return store.SignedOrderRecord{}, err
	}
	stored.State, stored.Revision = transition.To, stored.Revision+1
	if matched != "" {
		stored.MatchedShares = matched
	}
	if exchangeID != "" {
		stored.ExchangeOrderID = exchangeID
	}
	s.orders[key] = stored
	intent := s.intents[order.IntentID]
	marked := intent.Status == store.IntentStatusDeadline || intent.Status == store.IntentStatusSignalDrift
	if !marked || transition.To == statemachine.StateFilled {
		intent.Status = transition.To
		s.intents[order.IntentID] = intent
	}
	return stored, nil
}

func (s *memStore) TransitionOrder(_ context.Context, order store.SignedOrderRecord, event statemachine.Event, matched, exchangeID, _ string) (store.SignedOrderRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.move(order, event, matched, exchangeID)
}

func (s *memStore) ResignRetryOrder(_ context.Context, order store.SignedOrderRecord, payload []byte, hash, salt, exchangeID, _ string) (store.SignedOrderRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := orderKey{order.IntentID, order.ChildSequence}
	stored := s.orders[key]
	if stored.Revision != order.Revision || stored.State != statemachine.StateRetryPending {
		return store.SignedOrderRecord{}, store.ErrConflict
	}
	stored.SignedPayload, stored.SignedOrderHash, stored.Salt = payload, hash, salt
	s.orders[key] = stored
	return s.move(stored, statemachine.EventRetryStarted, "", exchangeID)
}

func (s *memStore) PendingLadders(context.Context) ([]string, error) {
	ids := s.laddersIn(store.LadderActive, store.LadderStopping)
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(ids, func(id string) bool {
		for key, order := range s.orders {
			if key.intentID == id && !statemachine.IsTerminal(order.State) {
				return true
			}
		}
		return false
	}), nil
}

func (s *memStore) PlacingLadders(context.Context) ([]string, error) {
	return s.laddersIn(store.LadderPlacing), nil
}

func (s *memStore) laddersIn(phases ...store.LadderPhase) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, phase := range s.phases {
		if slices.Contains(phases, phase) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *memStore) OrderByIntent(_ context.Context, id string, sequence int) (store.SignedOrderRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if order, ok := s.orders[orderKey{id, sequence}]; ok {
		return order, nil
	}
	return store.SignedOrderRecord{}, store.ErrNotFound
}

func (s *memStore) IntentOrders(_ context.Context, id string) ([]store.SignedOrderRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var orders []store.SignedOrderRecord
	for key, order := range s.orders {
		if key.intentID == id {
			orders = append(orders, order)
		}
	}
	slices.SortFunc(orders, func(a, b store.SignedOrderRecord) int { return a.ChildSequence - b.ChildSequence })
	return orders, nil
}

func (s *memStore) OpenOrders(ctx context.Context) ([]store.SignedOrderRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var orders []store.SignedOrderRecord
	for _, order := range s.orders {
		if !statemachine.IsTerminal(order.State) {
			orders = append(orders, order)
		}
	}
	return orders, nil
}

func (s *memStore) LadderPhase(_ context.Context, id string) (store.LadderPhase, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phases[id], nil
}

func (s *memStore) MoveLadderPhase(_ context.Context, id string, to store.LadderPhase, from ...store.LadderPhase) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(from, s.phases[id]) {
		return false, nil
	}
	s.phases[id] = to
	if hook := s.onPhase; hook != nil {
		s.mu.Unlock()
		hook(id, to)
		s.mu.Lock()
	}
	return true, nil
}

func (s *memStore) states(id string) map[int]statemachine.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int]statemachine.State{}
	for key, order := range s.orders {
		if key.intentID == id {
			out[key.child] = order.State
		}
	}
	return out
}

func (s *memStore) status(id string) statemachine.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.intents[id].Status
}

// ladderCLOB scripts the exchange per price: a level can cross the book a set
// number of times (or forever), or be refused outright.
type ladderCLOB struct {
	mu      sync.Mutex
	tick    float64
	minSize float64
	next    int64
	created map[string]clobclient.UserOrder
	posts   map[float64][]string
	cross   map[float64]int // remaining crosses; negative never rests
	reject  map[float64]error
	cancels []string
	// batchPosts and batchCancels are the size of each batch call.
	batchPosts   []int
	batchCancels []int
	// noBatch answers the batch endpoints 404, as an exchange without them.
	noBatch bool
}

func newLadderCLOB() *ladderCLOB {
	return &ladderCLOB{tick: 0.01, created: map[string]clobclient.UserOrder{}, posts: map[float64][]string{}, cross: map[float64]int{}, reject: map[float64]error{}}
}

func (c *ladderCLOB) TickSize(context.Context, string) (float64, error)     { return c.tick, nil }
func (c *ladderCLOB) MinOrderSize(context.Context, string) (float64, error) { return c.minSize, nil }
func (c *ladderCLOB) CreateOrder(_ context.Context, order clobclient.UserOrder) (clobclient.SignedOrderV2, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	id := fmt.Sprintf("order-%d", c.next)
	c.created[id] = order
	return clobclient.SignedOrderV2{OrderID: id, Salt: c.next}, nil
}

var errCross = &clobclient.OrderRejectedError{Message: "invalid post-only order: order crosses book"}

func (c *ladderCLOB) SubmitSignedOrder(_ context.Context, signed clobclient.SignedOrderV2, _ clobclient.OrderType, postOnly bool) (*clobclient.OrderResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	price := c.created[signed.OrderID].Price
	c.posts[price] = append(c.posts[price], signed.OrderID)
	if !postOnly {
		return nil, errors.New("ladder levels must be post-only")
	}
	if err := c.reject[price]; err != nil {
		return nil, err
	}
	if c.cross[price] != 0 {
		if c.cross[price] > 0 {
			c.cross[price]--
		}
		return nil, errCross
	}
	return &clobclient.OrderResponse{Success: true, OrderID: signed.OrderID, Status: "live"}, nil
}

func (c *ladderCLOB) CancelOrder(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancels = append(c.cancels, id)
	return nil
}

func (c *ladderCLOB) CancelOrderResult(_ context.Context, id string) (*clobclient.CancelResult, error) {
	return &clobclient.CancelResult{Canceled: []string{id}}, c.CancelOrder(context.Background(), id)
}

func (c *ladderCLOB) postCount(price float64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.posts[price])
}

func (c *ladderCLOB) totalPosts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, posts := range c.posts {
		n += len(posts)
	}
	return n
}

func (c *ladderCLOB) forever(prices ...float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, price := range prices {
		c.cross[price] = -1
	}
}

// resultSink records the open results executiond publishes.
type resultSink struct {
	mu      sync.Mutex
	results []protocol.ExecutionOpenResult
	fail    error
}

func (p *resultSink) PublishJSON(subject string, value any) error {
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

func (p *resultSink) published() []protocol.ExecutionOpenResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.results)
}

type ladderHarness struct {
	exec  *Executor
	store *memStore
	clob  *ladderCLOB
	sink  *resultSink
}

func newLadderHarness(t *testing.T) *ladderHarness {
	t.Helper()
	h := &ladderHarness{store: newMemStore(), clob: newLadderCLOB(), sink: &resultSink{}}
	exec, err := New(h.store, h.clob, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	exec.SetEventPublisher(h.sink)
	exec.SetErrorHandler(func(err error) { t.Logf("executor error: %v", err) })
	t.Cleanup(func() { _ = exec.Close(context.Background()) })
	h.exec = exec
	return h
}

func ladderIntent(id string) protocol.ExecutionIntent {
	return protocol.ExecutionIntent{
		SchemaVersion: protocol.SchemaVersionV1, IntentID: id, UniqueTag: "lane", Strategy: "strategy", Kind: protocol.IntentOpen,
		ConditionID: "cond", TokenID: "token", Outcome: "Up", Side: protocol.SideBuy, TargetUSD: "12",
		BottomPrice: "0.50", LimitPrice: "0.60", ScalingMode: protocol.ScalingModeFlat, TimeInForce: protocol.TimeInForceGTC, PostOnly: true,
		ExpiresAt: time.Now().Add(time.Minute), Policy: protocol.ExecutionPolicy{Style: protocol.ExecutionStyleMultiLevelLimit},
	}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func countStates(states map[int]statemachine.State, want statemachine.State) int {
	n := 0
	for _, state := range states {
		if state == want {
			n++
		}
	}
	return n
}

func TestParseLadder(t *testing.T) {
	for name, tc := range map[string]struct {
		edit    func(*protocol.ExecutionIntent)
		wantErr bool
		levels  int
	}{
		"eleven levels":     {edit: func(*protocol.ExecutionIntent) {}, levels: 11},
		"single level":      {edit: func(i *protocol.ExecutionIntent) { i.BottomPrice = "0.60" }, levels: 1},
		"off-cent bottom":   {edit: func(i *protocol.ExecutionIntent) { i.BottomPrice = "0.505" }, wantErr: true},
		"off-cent limit":    {edit: func(i *protocol.ExecutionIntent) { i.LimitPrice = "0.605" }, wantErr: true},
		"bottom above":      {edit: func(i *protocol.ExecutionIntent) { i.BottomPrice = "0.61" }, wantErr: true},
		"limit of one":      {edit: func(i *protocol.ExecutionIntent) { i.LimitPrice = "1.00" }, wantErr: true},
		"zero bottom":       {edit: func(i *protocol.ExecutionIntent) { i.BottomPrice = "0" }, wantErr: true},
		"missing bottom":    {edit: func(i *protocol.ExecutionIntent) { i.BottomPrice = "" }, wantErr: true},
		"scaling not flat":  {edit: func(i *protocol.ExecutionIntent) { i.ScalingMode = "pyramid" }, wantErr: true},
		"scaling missing":   {edit: func(i *protocol.ExecutionIntent) { i.ScalingMode = "" }, wantErr: true},
		"sell":              {edit: func(i *protocol.ExecutionIntent) { i.Side = protocol.SideSell }, wantErr: true},
		"not post-only":     {edit: func(i *protocol.ExecutionIntent) { i.PostOnly = false }, levels: 11},
		"not GTC":           {edit: func(i *protocol.ExecutionIntent) { i.TimeInForce = protocol.TimeInForceFAK }, wantErr: true},
		"long decimal tail": {edit: func(i *protocol.ExecutionIntent) { i.BottomPrice = "0.5000000001" }, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			intent := ladderIntent("i")
			tc.edit(&intent)
			spec, err := parseLadder(intent)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
			if err == nil && spec.levels() != tc.levels {
				t.Fatalf("levels=%d, want %d", spec.levels(), tc.levels)
			}
			if err == nil {
				return
			}
			var declared rejection
			if !errors.As(err, &declared) || declared.code != protocol.ReasonInvalidIntent {
				t.Fatalf("err=%v, want an INVALID_INTENT rejection", err)
			}
		})
	}
}

func TestValidateIntentAcceptsLadderWithoutRetryFlagAndRefusesItElsewhere(t *testing.T) {
	now := time.Now()
	intent := ladderIntent("i")
	intent.Policy.PostOnlyCrossRetry = true
	if err := validateIntentAt(intent, now); err != nil {
		t.Fatalf("ladder with the retry flag: %v", err)
	}
	intent = ladderIntent("i")
	intent.Policy.Style = protocol.ExecutionStyleLimit
	intent.Policy.PostOnlyCrossRetry = true
	if err := validateIntentAt(intent, now); err == nil {
		t.Fatal("post_only_cross_retry accepted for a single-order style")
	}
	intent = ladderIntent("i")
	intent.Policy.MaxReprices = 1
	if err := validateIntentAt(intent, now); err == nil {
		t.Fatal("repricing accepted for a ladder")
	}
}

func TestPlanLadderOneChildPerCentHighestFirstAtFullDollars(t *testing.T) {
	h := newLadderHarness(t)
	intent := ladderIntent("i")
	spec, _ := parseLadder(intent)
	children, err := h.exec.planLadder(context.Background(), intent, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 11 {
		t.Fatalf("children=%d, want 11", len(children))
	}
	for i, child := range children {
		cents := 60 - i
		if got, want := mustFloat(t, child.Price), float64(cents)/100; got != want {
			t.Fatalf("child %d price=%s, want %v", i, child.Price, want)
		}
		if child.Sequence != cents-50+1 {
			t.Fatalf("child %d sequence=%d, want %d (stable from the bottom)", i, child.Sequence, cents-50+1)
		}
		if !child.PostOnly || child.TimeInForce != protocol.TimeInForceGTC {
			t.Fatalf("child %d not a post-only GTC: %+v", i, child)
		}
		notional := mustFloat(t, child.Shares) * float64(cents) / 100
		if notional < 11.9 || notional > 12 {
			t.Fatalf("child %d notional=%f, want the whole 12 USD target", i, notional)
		}
	}
}

func mustFloat(t *testing.T, text string) float64 {
	t.Helper()
	var value float64
	if _, err := fmt.Sscanf(text, "%g", &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestPlanLadderSkipsLevelsUnderTheMinimumAndFailsOnlyWhenNoneIsLeft(t *testing.T) {
	h := newLadderHarness(t)
	intent := ladderIntent("i")
	spec, _ := parseLadder(intent)
	h.clob.minSize = 22 // 12 USD buys 22.2 shares at 0.54 and 21.8 at 0.55
	children, err := h.exec.planLadder(context.Background(), intent, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 5 || mustFloat(t, children[0].Price) != 0.54 || children[0].Sequence != 5 {
		t.Fatalf("children=%+v, want the five levels 0.54..0.50", children)
	}
	h.clob.minSize = 1000
	_, err = h.exec.planLadder(context.Background(), intent, spec)
	var declared rejection
	if !errors.As(err, &declared) || declared.code != protocol.ReasonBelowMinOrderSize {
		t.Fatalf("err=%v, want BELOW_MIN_ORDER_SIZE", err)
	}
}

func TestPlanLadderRefusesWideLaddersAndCoarseTicks(t *testing.T) {
	h := newLadderHarness(t)
	intent := ladderIntent("i")
	intent.BottomPrice, intent.LimitPrice = "0.10", "0.60"
	spec, _ := parseLadder(intent)
	if _, err := h.exec.planLadder(context.Background(), intent, spec); err == nil {
		t.Fatal("a 51-level ladder passed the default limit")
	}
	h.exec.SetMaxLadderLevels(51)
	if _, err := h.exec.planLadder(context.Background(), intent, spec); err != nil {
		t.Fatalf("raised limit refused: %v", err)
	}
	for _, tick := range []float64{0.1, 0.02, 0.003} {
		h.clob.tick = tick
		_, err := h.exec.planLadder(context.Background(), ladderIntent("i"), ladderSpec{50, 60})
		var declared rejection
		if !errors.As(err, &declared) || declared.code != protocol.ReasonUnplannable {
			t.Fatalf("tick %v: err=%v, want UNPLANNABLE", tick, err)
		}
	}
	for _, tick := range []float64{0.01, 0.001, 0.0001} {
		h.clob.tick = tick
		if _, err := h.exec.planLadder(context.Background(), ladderIntent("i"), ladderSpec{50, 60}); err != nil {
			t.Fatalf("tick %v refused: %v", tick, err)
		}
	}
}

func TestLadderPostsEveryLevelAndRepostsCrossedLevelsWithAFreshSignature(t *testing.T) {
	h := newLadderHarness(t)
	h.clob.cross[0.60], h.clob.cross[0.58] = 3, 1
	intent := ladderIntent("i")
	if err := h.exec.Execute(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if phase, _ := h.store.LadderPhase(context.Background(), "i"); phase == store.LadderPlacing {
		t.Fatal("placement finished but the phase is still PLACING")
	}
	eventually(t, "every level to rest", func() bool { return countStates(h.store.states("i"), statemachine.StateLive) == 11 })
	if got := h.clob.postCount(0.60); got != 4 {
		t.Fatalf("0.60 was posted %d times, want 4 (three crosses then a rest)", got)
	}
	if got := h.clob.postCount(0.58); got != 2 {
		t.Fatalf("0.58 was posted %d times, want 2", got)
	}
	if got := h.clob.postCount(0.55); got != 1 {
		t.Fatalf("an uncrossed level was posted %d times, want 1", got)
	}
	ids := h.clob.posts[0.60]
	for i := range ids {
		if slices.Index(ids, ids[i]) != i {
			t.Fatalf("a retry reposted a signature it had already sent: %v", ids)
		}
	}
	order, _ := h.store.OrderByIntent(context.Background(), "i", 11)
	if order.ExchangeOrderID != ids[len(ids)-1] {
		t.Fatalf("the row holds %s, but the order that rested is %s", order.ExchangeOrderID, ids[len(ids)-1])
	}
	eventually(t, "the retry goroutine to unregister", func() bool { return !h.exec.ladders.has("i") })
	if got := len(h.sink.published()); got != 0 {
		t.Fatalf("%d results published while every level still rests", got)
	}
}

func TestLadderRetriesBackOffWhileTheLevelKeepsCrossing(t *testing.T) {
	h := newLadderHarness(t)
	h.clob.forever(0.60)
	if err := h.exec.Execute(context.Background(), ladderIntent("i")); err != nil {
		t.Fatal(err)
	}
	// Doubling from 25 ms, reposts land near 25, 75, 175 and 375 ms: four in
	// 400 ms, where a fixed 25 ms pace would have sent sixteen.
	time.Sleep(400 * time.Millisecond)
	if posts := h.clob.postCount(0.60) - 1; posts < 2 || posts > 6 {
		t.Fatalf("%d reposts in 400ms; want 2..6 with backoff from %s", posts, ladderRetryInterval)
	}
}

func TestLadderNonCrossRefusalFailsOnlyThatLevel(t *testing.T) {
	h := newLadderHarness(t)
	h.clob.reject[0.55] = &clobclient.OrderRejectedError{Message: "not enough balance / allowance"}
	if err := h.exec.Execute(context.Background(), ladderIntent("i")); err != nil {
		t.Fatalf("one refused level failed the ladder: %v", err)
	}
	states := h.store.states("i")
	if states[6] != statemachine.StateRejected || countStates(states, statemachine.StateLive) != 10 {
		t.Fatalf("states=%v, want level 6 rejected and ten resting", states)
	}
	if h.clob.postCount(0.55) != 1 {
		t.Fatal("a refused level was reposted")
	}
}

func TestLadderFailsWhenNoLevelIsPlaced(t *testing.T) {
	h := newLadderHarness(t)
	for cents := 50; cents <= 60; cents++ {
		h.clob.reject[float64(cents)/100] = &clobclient.OrderRejectedError{Message: "market closed"}
	}
	err := h.exec.Execute(context.Background(), ladderIntent("i"))
	var declared rejection
	if !errors.As(err, &declared) || declared.code != protocol.ReasonOrderRejected {
		t.Fatalf("err=%v, want an ORDER_REJECTED rejection", err)
	}
	// The caller reports a ladder that placed nothing; the ladder must not.
	if got := len(h.sink.published()); got != 0 {
		t.Fatalf("%d aggregated results for a ladder whose caller reports the failure", got)
	}
	h.exec.publishLadder(context.Background(), "i", "late observer")
	if got := len(h.sink.published()); got != 0 {
		t.Fatalf("a later observer published a result for a failed ladder (%d)", got)
	}
}

func TestCancelStopsRetriesAndPublishesOneResult(t *testing.T) {
	h := newLadderHarness(t)
	h.clob.forever(0.60, 0.59)
	if err := h.exec.Execute(context.Background(), ladderIntent("i")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the crossed levels to be retried", func() bool { return h.clob.postCount(0.60) >= 3 })

	req := protocol.ExecutionCloseRequest{ConditionID: "cond", AssetID: "token", UniqueTag: "lane"}
	if err := h.exec.cancelLaneOpenOrders(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if h.exec.ladders.has("i") {
		t.Fatal("the ladder is still registered after the cancel")
	}
	settled := h.clob.totalPosts()
	time.Sleep(150 * time.Millisecond)
	if h.clob.totalPosts() != settled {
		t.Fatal("a level was posted after the cancel returned")
	}
	states := h.store.states("i")
	if states[11] != statemachine.StateCanceled || states[10] != statemachine.StateCanceled {
		t.Fatalf("parked levels not cancelled locally: %v", states)
	}
	if got := len(h.clob.cancels); got != 9 {
		t.Fatalf("%d exchange cancels, want one for each of the nine resting levels", got)
	}
	if phase, _ := h.store.LadderPhase(context.Background(), "i"); phase != store.LadderStopping {
		t.Fatalf("phase=%s, want STOPPING", phase)
	}
	if got := len(h.sink.published()); got != 0 {
		t.Fatalf("%d results before the exchange confirmed the cancels", got)
	}
	// The account feed confirms each cancel; observers race to publish.
	for sequence, state := range h.store.states("i") {
		if state == statemachine.StateCanceled {
			continue
		}
		order, _ := h.store.OrderByIntent(context.Background(), "i", sequence)
		if _, err := h.store.TransitionOrder(context.Background(), order, statemachine.EventCancelObserved, "0", "", "feed"); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); h.exec.publishLadder(context.Background(), "i", "feed") }()
	}
	wg.Wait()
	results := h.sink.published()
	if len(results) != 1 {
		t.Fatalf("%d results published, want exactly one", len(results))
	}
	if r := results[0]; r.Status != protocol.ResultFailed || r.ReasonCode != protocol.ReasonCanceled || r.FilledShares != 0 {
		t.Fatalf("result=%+v, want a failed CANCELED result", r)
	}
	if phase, _ := h.store.LadderPhase(context.Background(), "i"); phase != store.LadderPublished {
		t.Fatalf("phase=%s, want PUBLISHED", phase)
	}
}

func TestCancelledLateLevelsDoNotClobberTheStopMarker(t *testing.T) {
	// Each child transition rewrites the intent status; the stop marker lives
	// in the phase column so it survives them.
	h := newLadderHarness(t)
	h.clob.forever(0.60)
	if err := h.exec.Execute(context.Background(), ladderIntent("i")); err != nil {
		t.Fatal(err)
	}
	intent, _ := h.store.Intent(context.Background(), "i")
	if err := h.exec.stopLadder(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	order, _ := h.store.OrderByIntent(context.Background(), "i", 1)
	if _, err := h.store.TransitionOrder(context.Background(), order, statemachine.EventCancelRequested, "0", "", "x"); err != nil {
		t.Fatal(err)
	}
	if phase, _ := h.store.LadderPhase(context.Background(), "i"); phase != store.LadderStopping {
		t.Fatalf("phase=%s after a child moved, want STOPPING", phase)
	}
}

func TestLadderRetriesEndAtTheDeadlineAndPublishOneExpiredResult(t *testing.T) {
	h := newLadderHarness(t)
	for cents := 50; cents <= 60; cents++ {
		h.clob.forever(float64(cents) / 100)
	}
	intent := ladderIntent("i")
	intent.ExpiresAt = time.Now().Add(200 * time.Millisecond)
	if err := h.exec.Execute(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the deadline result", func() bool { return len(h.sink.published()) == 1 })
	time.Sleep(100 * time.Millisecond)
	results := h.sink.published()
	if len(results) != 1 || results[0].ReasonCode != protocol.ReasonExpired || results[0].Status != protocol.ResultFailed {
		t.Fatalf("results=%+v, want one failed EXPIRED result", results)
	}
	if got := countStates(h.store.states("i"), statemachine.StateCanceled); got != 11 {
		t.Fatalf("%d levels cancelled, want all 11", got)
	}
	if h.store.status("i") != store.IntentStatusDeadline {
		t.Fatalf("intent status=%s, want the deadline marker", h.store.status("i"))
	}
	before := h.clob.totalPosts()
	time.Sleep(100 * time.Millisecond)
	if h.clob.totalPosts() != before {
		t.Fatal("levels were reposted after the deadline")
	}
}

func TestAdoptLaddersResumesParkedLevelsAfterARestart(t *testing.T) {
	h := newLadderHarness(t)
	intent := ladderIntent("i")
	record := mapping.IntentRecord(intent, time.Now())
	h.store.intents["i"] = record
	h.store.phases["i"] = store.LadderPlacing // the old process died mid-placement
	parked := store.SignedOrderRecord{IntentID: "i", ChildSequence: 11, SignedPayload: []byte(`{}`), SignedOrderHash: "old", Salt: "1", ExchangeOrderID: "old-id",
		RequestedShares: "20", MatchedShares: "0", Price: "0.60", OrderType: store.TimeInForceGTC, PostOnly: true, State: statemachine.StateRetryPending, Revision: 5}
	h.store.orders[orderKey{"i", 11}] = parked
	h.clob.cross[0.60] = 2
	open, _ := h.store.OpenOrders(context.Background())
	if err := h.exec.adoptLadders(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	if phase, _ := h.store.LadderPhase(context.Background(), "i"); phase != store.LadderActive {
		t.Fatalf("phase=%s, want ACTIVE after adoption", phase)
	}
	eventually(t, "the parked level to rest", func() bool { return h.store.states("i")[11] == statemachine.StateLive })
	if h.clob.postCount(0.60) != 3 {
		t.Fatalf("posted %d times, want 3", h.clob.postCount(0.60))
	}
	// A second pass must not start a second goroutine for the same ladder.
	if err := h.exec.adoptLadders(context.Background(), open); err != nil {
		t.Fatal(err)
	}
}

func TestCloseStopsRunningLadders(t *testing.T) {
	h := newLadderHarness(t)
	h.clob.forever(0.60)
	if err := h.exec.Execute(context.Background(), ladderIntent("i")); err != nil {
		t.Fatal(err)
	}
	if err := h.exec.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	settled := h.clob.totalPosts()
	time.Sleep(100 * time.Millisecond)
	if h.clob.totalPosts() != settled {
		t.Fatal("a ladder kept posting after Close")
	}
	if err := h.exec.Execute(context.Background(), ladderIntent("j")); err == nil {
		t.Fatal("a closed executor accepted a ladder")
	}
}

func TestIsPostOnlyCross(t *testing.T) {
	for reason, want := range map[string]bool{
		"invalid post-only order: order crosses book": true,
		// Observed live 2026-11-08 (cmd/postonlyprobe): HTTP 400, not success=false.
		`HTTP 400 for POST /order: {"error":"invalid post-only order: order crosses book"}`: true,
		"INVALID_POST_ONLY_ORDER: order would cross":                                        true,
		"post only order is marketable":                                                     true,
		"not enough balance / allowance":                                                    false,
		"order crosses book":                                                                false, // not post-only: say nothing about retrying
		"invalid post-only order: price off tick grid":                                      false,
		"the maker address is not allowed (post only)":                                      false,
		"": false,
	} {
		if got := isPostOnlyCross(reason); got != want {
			t.Errorf("isPostOnlyCross(%q)=%v, want %v", reason, got, want)
		}
	}
}

func TestLaneScanIncludesEveryLadderLevelButNotOrdinaryInternalChildren(t *testing.T) {
	h := newLadderHarness(t)
	if err := h.exec.Execute(context.Background(), ladderIntent("ladder")); err != nil {
		t.Fatal(err)
	}
	// An ordinary open with an internal child (a force-close exit) on the lane.
	ordinary := mapping.IntentRecord(protocol.ExecutionIntent{
		SchemaVersion: protocol.SchemaVersionV1, IntentID: "ordinary", UniqueTag: "lane", Strategy: "strategy", Kind: protocol.IntentOpen,
		ConditionID: "cond", TokenID: "token", Outcome: "Up", Side: protocol.SideBuy, LimitPrice: "0.5", TimeInForce: protocol.TimeInForceGTC,
		Policy: protocol.ExecutionPolicy{Style: protocol.ExecutionStyleLimit},
	}, time.Now())
	h.store.intents["ordinary"] = ordinary
	for _, sequence := range []int{store.StrategyChildSequence, forceCloseChildSequence} {
		h.store.orders[orderKey{"ordinary", sequence}] = store.SignedOrderRecord{IntentID: "ordinary", ChildSequence: sequence, State: statemachine.StateLive, Revision: 1, MatchedShares: "0"}
	}
	lane, err := h.exec.laneActiveOrders(context.Background(), "cond", "token", "lane")
	if err != nil {
		t.Fatal(err)
	}
	perIntent := map[string][]int{}
	for _, candidate := range lane {
		perIntent[candidate.intent.IntentID] = append(perIntent[candidate.intent.IntentID], candidate.order.ChildSequence)
	}
	if len(perIntent["ladder"]) != 11 {
		t.Fatalf("lane holds %d ladder levels, want all 11", len(perIntent["ladder"]))
	}
	if got := perIntent["ordinary"]; len(got) != 1 || got[0] != store.StrategyChildSequence {
		t.Fatalf("lane holds ordinary children %v, want only the strategy child", got)
	}
}

func TestSupersedingCloseStopsTheLadderAndCancelsEveryLevel(t *testing.T) {
	h := newLadderHarness(t)
	h.clob.forever(0.60)
	if err := h.exec.Execute(context.Background(), ladderIntent("i")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the crossed level to be retried", func() bool { return h.clob.postCount(0.60) >= 2 })
	lane, err := h.exec.laneActiveOrders(context.Background(), "cond", "token", "lane")
	if err != nil {
		t.Fatal(err)
	}
	// As replaceCloseLane does: level intents are gathered and cancelled
	// together, never one at a time through replaceStaleChild.
	ladders := laneLadders{}
	for _, candidate := range lane {
		if !supersededBy(protocol.ExecutionCloseModeForce, candidate) {
			t.Fatal("a force close should retire the ladder's levels")
		}
		ladders.add(candidate)
	}
	if err := h.exec.cancelLaneLadders(context.Background(), ladders, map[string]bool{}, "multi-level intent superseded by a close"); err != nil {
		t.Fatal(err)
	}
	settled := h.clob.totalPosts()
	time.Sleep(120 * time.Millisecond)
	if h.clob.totalPosts() != settled {
		t.Fatal("the ladder kept posting after the force close superseded it")
	}
	states := h.store.states("i")
	if states[11] != statemachine.StateCanceled {
		t.Fatalf("parked level state=%s, want cancelled locally", states[11])
	}
	if len(h.clob.cancels) != 10 {
		t.Fatalf("%d exchange cancels, want one per resting level (10)", len(h.clob.cancels))
	}
	if phase, _ := h.store.LadderPhase(context.Background(), "i"); phase != store.LadderStopping {
		t.Fatalf("phase=%s, want STOPPING", phase)
	}
}

func TestCancelWithOnlyParkedLevelsPublishesAtOnce(t *testing.T) {
	// Every level crossed, so none is on the exchange to confirm a cancel.
	h := newLadderHarness(t)
	for cents := 50; cents <= 60; cents++ {
		h.clob.forever(float64(cents) / 100)
	}
	if err := h.exec.Execute(context.Background(), ladderIntent("i")); err != nil {
		t.Fatal(err)
	}
	req := protocol.ExecutionCloseRequest{ConditionID: "cond", AssetID: "token", UniqueTag: "lane"}
	if err := h.exec.cancelLaneOpenOrders(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	results := h.sink.published()
	if len(results) != 1 || results[0].ReasonCode != protocol.ReasonCanceled || results[0].Status != protocol.ResultFailed {
		t.Fatalf("results=%+v, want one failed CANCELED result straight from the cancel", results)
	}
	if len(h.clob.cancels) != 0 {
		t.Fatalf("%d exchange cancels for orders that never reached the exchange", len(h.clob.cancels))
	}
	if err := h.exec.cancelLaneOpenOrders(context.Background(), req); err != nil || len(h.sink.published()) != 1 {
		t.Fatalf("a repeated cancel published again (err=%v)", err)
	}
}

func TestRecoveryPublishesAResultALadderWasOwedWhenTheProcessDied(t *testing.T) {
	// Every level ended and the phase says placement did, but the process died
	// before it published: no child will move again to trigger it.
	h := newLadderHarness(t)
	h.store.intents["i"] = mapping.IntentRecord(ladderIntent("i"), time.Now())
	h.store.phases["i"] = store.LadderActive
	for sequence, state := range map[int]statemachine.State{1: statemachine.StateCanceled, 2: statemachine.StateFilled} {
		matched := "0"
		if state == statemachine.StateFilled {
			matched = "10"
		}
		h.store.orders[orderKey{"i", sequence}] = store.SignedOrderRecord{IntentID: "i", ChildSequence: sequence, State: state, MatchedShares: matched, Price: "0.55", ExchangeOrderID: "x", Revision: 3}
	}
	for range 3 {
		if err := h.exec.recoverOpenOrders(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	results := h.sink.published()
	if len(results) != 1 || results[0].Status != protocol.ResultSucceeded || results[0].FilledShares != 10 {
		t.Fatalf("results=%+v, want the one owed result, published once across three passes", results)
	}
	if phase, _ := h.store.LadderPhase(context.Background(), "i"); phase != store.LadderPublished {
		t.Fatalf("phase=%s, want PUBLISHED", phase)
	}
}

func parkedRow(id string, sequence int, price string) store.SignedOrderRecord {
	return store.SignedOrderRecord{IntentID: id, ChildSequence: sequence, SignedPayload: []byte(`{}`), SignedOrderHash: "old", Salt: "1", ExchangeOrderID: "old-id",
		RequestedShares: "20", MatchedShares: "0", Price: price, OrderType: store.TimeInForceGTC, PostOnly: true, State: statemachine.StateRetryPending, Revision: 5}
}

func TestAdoptLaddersCancelsParkedLevelsOfAStoppedLadderInsteadOfReposting(t *testing.T) {
	// The process died after a CANCEL_OPEN marked the ladder STOPPING but
	// before every parked level was cancelled.
	h := newLadderHarness(t)
	h.store.intents["i"] = mapping.IntentRecord(ladderIntent("i"), time.Now())
	h.store.phases["i"] = store.LadderStopping
	h.store.orders[orderKey{"i", 11}] = parkedRow("i", 11, "0.60")
	if err := h.exec.recoverOpenOrders(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := h.clob.totalPosts(); n != 0 {
		t.Fatalf("a stopped ladder posted %d orders after the restart", n)
	}
	if state := h.store.states("i")[11]; state != statemachine.StateCanceled {
		t.Fatalf("parked level is %s, want CANCELED", state)
	}
	if results := h.sink.published(); len(results) != 1 || results[0].ReasonCode != protocol.ReasonCanceled {
		t.Fatalf("results=%+v, want one CANCELED result", results)
	}
}

func TestRecoveryEndsAPlacementWhoseSavedLevelsAllEnded(t *testing.T) {
	// The process died mid-placement after every saved level had ended, so no
	// open order points adoption at the ladder.
	h := newLadderHarness(t)
	h.store.intents["i"] = mapping.IntentRecord(ladderIntent("i"), time.Now())
	h.store.phases["i"] = store.LadderPlacing
	h.store.orders[orderKey{"i", 1}] = store.SignedOrderRecord{IntentID: "i", ChildSequence: 1, State: statemachine.StateFilled, MatchedShares: "10", Price: "0.55", ExchangeOrderID: "x", Revision: 3}
	for range 2 {
		if err := h.exec.recoverOpenOrders(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if results := h.sink.published(); len(results) != 1 || results[0].FilledShares != 10 {
		t.Fatalf("results=%+v, want the one result the stranded ladder owed", results)
	}
}

func TestCancelTakenBeforeAnyLevelWasSavedStopsTheLadder(t *testing.T) {
	// The CANCEL_OPEN came in while the ladder was planning: the lane scan
	// found nothing, and only the post gate knows.
	h := newLadderHarness(t)
	for cents := 50; cents <= 60; cents++ {
		h.clob.forever(float64(cents) / 100)
	}
	ctx := WithPostGate(context.Background(), func() bool { return true })
	if err := h.exec.Execute(ctx, ladderIntent("i")); err != nil {
		t.Fatal(err)
	}
	if h.exec.ladders.has("i") {
		t.Fatal("a dropped ladder is still running its retries")
	}
	before := h.clob.totalPosts()
	time.Sleep(150 * time.Millisecond)
	if h.clob.totalPosts() != before {
		t.Fatal("a dropped ladder reposted its parked levels")
	}
	if got := countStates(h.store.states("i"), statemachine.StateCanceled); got != 11 {
		t.Fatalf("%d levels cancelled, want all 11", got)
	}
	if results := h.sink.published(); len(results) != 1 || results[0].ReasonCode != protocol.ReasonCanceled {
		t.Fatalf("results=%+v, want one CANCELED result", results)
	}
}

func TestLadderStoppedBeforeAnyLevelWasReservedIsRejected(t *testing.T) {
	// The stop lands between PLACING and the first reservation: nothing is
	// placed or parked, and the open must still get its one result.
	h := newLadderHarness(t)
	h.store.onPhase = func(id string, to store.LadderPhase) {
		if to == store.LadderPlacing {
			// stop's cancel without its wait: the run is the caller.
			h.exec.ladders.mu.Lock()
			run := h.exec.ladders.runs[id]
			h.exec.ladders.mu.Unlock()
			run.cancel()
		}
	}
	// Execute's error is what ExecuteOpen publishes as the rejection.
	if err := h.exec.Execute(context.Background(), ladderIntent("i")); err == nil {
		t.Fatal("a ladder stopped before placement returned no error, so no result is published")
	}
	if got := h.clob.totalPosts(); got != 0 {
		t.Fatalf("%d posts from a ladder stopped before placement", got)
	}
	if results := h.sink.published(); len(results) != 0 {
		t.Fatalf("results=%+v, want none from the ladder itself", results)
	}
}

func TestRecoverySweepsAParkedLevelAFailedCancelLeftBehind(t *testing.T) {
	h := newLadderHarness(t)
	h.store.intents["i"] = mapping.IntentRecord(ladderIntent("i"), time.Now())
	h.store.phases["i"] = store.LadderStopping
	h.exec.laddersAdopted.Store(true) // adoption ran long ago; this row is a leftover
	h.store.orders[orderKey{"i", 11}] = parkedRow("i", 11, "0.60")
	if err := h.exec.recoverOpenOrders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := h.store.states("i")[11]; state != statemachine.StateCanceled {
		t.Fatalf("leftover parked level is %s, want CANCELED", state)
	}
	if n := h.clob.totalPosts(); n != 0 {
		t.Fatalf("the sweep posted %d orders", n)
	}
	if results := h.sink.published(); len(results) != 1 {
		t.Fatalf("results=%+v, want the one result the leftover held back", results)
	}
}

func TestFailedLadderIsClaimedBeforeAnyObserverCanPublish(t *testing.T) {
	h := newLadderHarness(t)
	for cents := 50; cents <= 60; cents++ {
		h.clob.reject[float64(cents)/100] = &clobclient.OrderRejectedError{Message: "market closed"}
	}
	if err := h.exec.Execute(context.Background(), ladderIntent("i")); err == nil {
		t.Fatal("a ladder that placed nothing succeeded")
	}
	if phase, _ := h.store.LadderPhase(context.Background(), "i"); phase != store.LadderPublished {
		t.Fatalf("phase=%s, want PUBLISHED straight from PLACING", phase)
	}
	if err := h.exec.recoverOpenOrders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(h.sink.published()); got != 0 {
		t.Fatalf("recovery published %d results for a ladder its caller reports", got)
	}
}

func TestLadderReservesTheLevelsNearestTheMarketFirst(t *testing.T) {
	// Exposure covers three of eleven levels: they must be the top three,
	// not whichever three posts won a race.
	for range 20 {
		h := newLadderHarness(t)
		h.store.buyBudget = 3
		if err := h.exec.Execute(context.Background(), ladderIntent("i")); err != nil {
			t.Fatal(err)
		}
		posted := map[float64]bool{}
		for _, price := range []float64{0.60, 0.59, 0.58} {
			posted[price] = h.clob.postCount(price) == 1
		}
		if !posted[0.60] || !posted[0.59] || !posted[0.58] || h.clob.totalPosts() != 3 {
			t.Fatalf("posted %v of 0.58-0.60 and %d in all, want exactly the top three", posted, h.clob.totalPosts())
		}
	}
}
