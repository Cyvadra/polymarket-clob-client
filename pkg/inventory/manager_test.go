package inventory

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// memStore is an InventoryStore over one lane, enough to drive the manager.
type memStore struct {
	mu       sync.Mutex
	held     float64 // pairs booked
	ops      map[string]*store.CTFOperationRecord
	order    []string
	merged   bool
	reserved float64
	// confirmFails fails that many ConfirmCTFOperation calls first.
	confirmFails int
	// traded is the wallet's net exchange fills per token, in base units.
	traded map[string]int64
}

func newMemStore() *memStore { return &memStore{ops: map[string]*store.CTFOperationRecord{}} }

func (s *memStore) level() store.InventoryLevel {
	inflight := 0.0
	for _, op := range s.ops {
		if op.Action == store.CTFSplit && (op.State == store.CTFPending || op.State == store.CTFSubmitted || op.State == store.CTFUnknown) {
			inflight += toFloat(op.Shares)
		}
	}
	return store.InventoryLevel{Remaining: f(s.held - s.reserved), Reserved: f(s.reserved), InFlight: f(inflight), Merged: s.merged}
}

func f(v float64) string { return big.NewFloat(v).Text('f', 6) }

func (s *memStore) BeginSplit(_ context.Context, op store.CTFOperationRecord, maxShares string) (store.CTFOperationRecord, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.merged {
		return op, "", false, store.ErrInventoryClosed
	}
	level := s.level()
	gap := toFloat(op.TargetShares) - toFloat(level.Remaining) - toFloat(level.InFlight)
	if gap < 0 {
		gap = 0
	}
	shares := gap
	if maxShares != "" && toFloat(maxShares) < shares {
		shares = toFloat(maxShares)
	}
	if shares <= 0 {
		return op, f(gap), false, nil
	}
	op.Action, op.Shares, op.State = store.CTFSplit, f(shares), store.CTFPending
	s.ops[op.OpID] = &op
	s.order = append(s.order, op.OpID)
	return op, f(gap), true, nil
}

func (s *memStore) CloseInventory(_ context.Context, op store.CTFOperationRecord) (store.CTFOperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.merged = true
	return op, nil
}

func (s *memStore) BeginMerge(_ context.Context, op store.CTFOperationRecord) (store.CTFOperationRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	shares := s.held - s.reserved
	if op.Shares != "" && toFloat(op.Shares) < shares {
		shares = toFloat(op.Shares)
	}
	if shares <= 0 {
		return op, false, nil
	}
	op.Action, op.Shares, op.State = store.CTFMerge, f(shares), store.CTFPending
	s.ops[op.OpID] = &op
	s.merged = true
	return op, true, nil
}

func (s *memStore) ArmCTFOperation(_ context.Context, opID string, baseline []string, armedAt, deadline time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops[opID].Baseline, s.ops[opID].ArmedAt, s.ops[opID].Deadline = baseline, armedAt, deadline
	return nil
}

func (s *memStore) ExchangeNetShares(_ context.Context, tokens []string, _ time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(tokens))
	for i, token := range tokens {
		out[i] = big.NewInt(s.traded[token]).String()
	}
	return out, nil
}

func (s *memStore) MarkCTFOperation(_ context.Context, opID string, state store.CTFOpState, txID, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops[opID].State = state
	if txID != "" {
		s.ops[opID].RelayerTxID = txID
	}
	return nil
}

func (s *memStore) ConfirmCTFOperation(_ context.Context, opID, _ string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.confirmFails > 0 {
		s.confirmFails--
		return errors.New("database unavailable")
	}
	op := s.ops[opID]
	if op.State == store.CTFConfirmed {
		return nil
	}
	if op.Action == store.CTFSplit {
		s.held += toFloat(op.Shares)
	} else {
		s.held -= toFloat(op.Shares)
	}
	op.State = store.CTFConfirmed
	return nil
}

func (s *memStore) OpenCTFOperations(context.Context) ([]store.CTFOperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.CTFOperationRecord
	for _, op := range s.ops {
		if op.State == store.CTFPending || op.State == store.CTFSubmitted || op.State == store.CTFUnknown {
			out = append(out, *op)
		}
	}
	return out, nil
}

func (s *memStore) InventoryLevel(context.Context, string, string) (store.InventoryLevel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.level(), nil
}

func (s *memStore) CTFOperationsSettling(context.Context, time.Time) (bool, error) { return false, nil }

func (s *memStore) state(opID string) store.CTFOpState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ops[opID].State
}

// fakeChain lands every batch it accepts, moving both token balances.
type fakeChain struct {
	mu        sync.Mutex
	balances  map[string]*big.Int
	submitErr error
	// lands decides whether an unknown submission reached the chain.
	lands      bool
	submitted  int
	inBatch    atomic.Int32
	maxInBatch atomic.Int32
	confirm    time.Duration
	refreshed  []string
	// statusErr fails every relayer status read.
	statusErr error
	// trade moves balances as the batch is submitted, as other lanes'
	// exchange fills would, by token in base units.
	trade map[string]int64
}

func newFakeChain() *fakeChain {
	return &fakeChain{balances: map[string]*big.Int{"up": big.NewInt(0), "down": big.NewInt(0)}, lands: true}
}

func (c *fakeChain) Market(context.Context, string) (*clobclient.Market, error) {
	return &clobclient.Market{Tokens: []clobclient.Token{{TokenID: "up", Outcome: "Up"}, {TokenID: "down", Outcome: "Down"}}}, nil
}

func (c *fakeChain) SplitCalls(_ context.Context, _ string, amount *big.Int) ([]clobclient.WalletCall, *clobclient.Market, error) {
	return []clobclient.WalletCall{{Data: "split:" + amount.String()}}, nil, nil
}

func (c *fakeChain) MergeCalls(_ context.Context, _ string, amount *big.Int) ([]clobclient.WalletCall, *clobclient.Market, error) {
	return []clobclient.WalletCall{{Data: "merge:" + amount.String()}}, nil, nil
}

func (c *fakeChain) ExecuteWalletBatch(_ context.Context, calls []clobclient.WalletCall, _ string, _ time.Time) (*clobclient.RelayerTransaction, error) {
	if n := c.inBatch.Add(1); n > c.maxInBatch.Load() {
		c.maxInBatch.Store(n)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.submitted++
	for token, shares := range c.trade {
		c.balances[token].Add(c.balances[token], big.NewInt(shares))
	}
	if c.submitErr != nil && !(errors.Is(c.submitErr, clobclient.ErrSubmitOutcomeUnknown) && c.lands) {
		c.inBatch.Add(-1)
		return nil, c.submitErr
	}
	var amount big.Int
	data := calls[0].Data
	split := data[:6] == "split:"
	amount.SetString(data[6:], 10)
	for _, token := range []string{"up", "down"} {
		if split {
			c.balances[token].Add(c.balances[token], &amount)
		} else {
			c.balances[token].Sub(c.balances[token], &amount)
		}
	}
	if c.submitErr != nil {
		c.inBatch.Add(-1)
		return nil, c.submitErr
	}
	return &clobclient.RelayerTransaction{ID: "tx", State: "STATE_NEW"}, nil
}

func (c *fakeChain) RelayerTransactionStatus(context.Context, string) (*clobclient.RelayerTransaction, error) {
	if c.statusErr != nil {
		return nil, c.statusErr
	}
	if c.confirm > 0 {
		time.Sleep(c.confirm)
	}
	defer c.inBatch.Add(-1)
	return &clobclient.RelayerTransaction{ID: "tx", Hash: "0xhash", State: "STATE_CONFIRMED"}, nil
}

func (c *fakeChain) TokenBalance(_ context.Context, token string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.balances[token].String(), nil
}

func (c *fakeChain) UpdateBalanceAllowance(_ context.Context, assetType, _ string) error {
	c.mu.Lock()
	c.refreshed = append(c.refreshed, assetType)
	c.mu.Unlock()
	return nil
}

type nopCanceler struct{ calls atomic.Int32 }

func (n *nopCanceler) CancelInventoryOrders(context.Context, string, string, []string) error {
	n.calls.Add(1)
	return nil
}

type results struct {
	mu  sync.Mutex
	out []protocol.ExecutionInventoryResult
}

func (r *results) PublishJSON(_ string, payload any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.out = append(r.out, payload.(protocol.ExecutionInventoryResult))
	return nil
}

func (r *results) last() protocol.ExecutionInventoryResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out[len(r.out)-1]
}

func newTestManager(t *testing.T, chain *fakeChain, st *memStore, pub *results) *Manager {
	t.Helper()
	m, err := New(Config{Chain: chain, Store: st, Canceler: &nopCanceler{}, Publish: pub, Poll: time.Millisecond, MergeSettle: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func request(action protocol.InventoryAction, target string) protocol.ExecutionInventoryRequest {
	return protocol.ExecutionInventoryRequest{SchemaVersion: protocol.SchemaVersionV1, RequestID: "r", UniqueTag: "lane", Strategy: "s", ConditionID: "c", Action: action, TargetShares: target}
}

func TestSplitTopsUpToTargetAndRefills(t *testing.T) {
	chain, st, pub := newFakeChain(), newMemStore(), &results{}
	m := newTestManager(t, chain, st, pub)
	ctx := context.Background()
	if err := m.ExecuteInventory(ctx, request(protocol.InventorySplit, "100")); err != nil {
		t.Fatal(err)
	}
	if r := pub.last(); r.Status != protocol.ResultSucceeded || r.Shares != 100 || r.RemainingShares != 100 || r.TxHash != "0xhash" {
		t.Fatalf("first split: %+v", r)
	}
	// A resend splits nothing.
	if err := m.ExecuteInventory(ctx, request(protocol.InventorySplit, "100")); err != nil {
		t.Fatal(err)
	}
	if r := pub.last(); r.Status != protocol.ResultSucceeded || r.Shares != 0 || chain.submitted != 1 {
		t.Fatalf("resent split: %+v after %d submissions", r, chain.submitted)
	}
	// An entry used 60; the same target refills exactly that.
	st.mu.Lock()
	st.held -= 60
	st.mu.Unlock()
	if err := m.ExecuteInventory(ctx, request(protocol.InventorySplit, "100")); err != nil {
		t.Fatal(err)
	}
	if r := pub.last(); r.Shares != 60 || r.RemainingShares != 100 {
		t.Fatalf("refill: %+v", r)
	}
}

func TestSplitIsCappedByFreeCash(t *testing.T) {
	chain, st, pub := newFakeChain(), newMemStore(), &results{}
	m := newTestManager(t, chain, st, pub)
	var wanted float64
	var claimed, released int
	m.cfg.ClaimCash = func(_ context.Context, want float64) (float64, func(), error) {
		wanted, claimed = want, claimed+1
		return 40, func() { released++ }, nil
	}
	if err := m.ExecuteInventory(context.Background(), request(protocol.InventorySplit, "100")); err != nil {
		t.Fatal(err)
	}
	if wanted != 100 || claimed != 1 || released != 1 {
		t.Fatalf("claim: wanted %v, claimed %d, released %d", wanted, claimed, released)
	}
	if r := pub.last(); r.Status != protocol.ResultSucceeded || r.ReasonCode != protocol.ReasonInventoryCapped || decimal.Compare(f(r.Shares), "40") > 0 || r.Shares < 39.99 {
		t.Fatalf("capped split: %+v", r)
	}
}

func TestUnknownSubmissionResolvesFromTheChainWithoutResubmitting(t *testing.T) {
	for _, landed := range []bool{true, false} {
		chain, st, pub := newFakeChain(), newMemStore(), &results{}
		chain.submitErr, chain.lands = clobclient.ErrSubmitOutcomeUnknown, landed
		m := newTestManager(t, chain, st, pub)
		m.cfg.BatchTTL = -time.Hour // already past: a batch that did not land fails at once
		now := time.Now()
		m.cfg.Now = func() time.Time { return now.Add(2 * time.Minute) }
		_ = m.ExecuteInventory(context.Background(), request(protocol.InventorySplit, "10"))
		if r := pub.last(); r.ReasonCode != protocol.ReasonRelayerUnknown {
			t.Fatalf("landed=%v: result %+v", landed, r)
		}
		if err := m.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := store.CTFFailed
		if landed {
			want = store.CTFConfirmed
		}
		if got := st.state(st.order[0]); got != want || chain.submitted != 1 {
			t.Fatalf("landed=%v: state %s after %d submissions, want %s after 1", landed, got, chain.submitted, want)
		}
	}
}

// Other lanes trade the condition's tokens while a blind operation is
// resolved: their fills are taken out of the balance moves, so a split that
// landed under a sell still confirms, and two buys do not pass for a split.
func TestBlindResolutionDiscountsTheWalletsTrades(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lands bool
		trade map[string]int64
		want  store.CTFOpState
	}{
		{"landed under a sell", true, map[string]int64{"down": -3_000_000}, store.CTFConfirmed},
		{"not landed under two buys", false, map[string]int64{"up": 10_000_000, "down": 10_000_000}, store.CTFFailed},
	} {
		chain, st, pub := newFakeChain(), newMemStore(), &results{}
		chain.submitErr, chain.lands, chain.trade = clobclient.ErrSubmitOutcomeUnknown, tc.lands, tc.trade
		st.traded = tc.trade
		m := newTestManager(t, chain, st, pub)
		m.cfg.BatchTTL = -time.Hour
		now := time.Now()
		m.cfg.Now = func() time.Time { return now.Add(2 * time.Minute) }
		_ = m.ExecuteInventory(context.Background(), request(protocol.InventorySplit, "10"))
		if err := m.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := st.state(st.order[0]); got != tc.want {
			t.Fatalf("%s: state %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestSubmissionsShareTheWalletOneAtATime(t *testing.T) {
	chain, pub := newFakeChain(), &results{}
	chain.confirm = 5 * time.Millisecond
	stores := []*memStore{newMemStore(), newMemStore(), newMemStore()}
	m := newTestManager(t, chain, stores[0], pub)
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.ExecuteInventory(context.Background(), request(protocol.InventorySplit, "1"+string(rune('0'+i))))
		}()
	}
	wg.Wait()
	if chain.maxInBatch.Load() != 1 {
		t.Fatalf("%d batches were in flight at once", chain.maxInBatch.Load())
	}
}

func TestMergeCancelsPreservesAndClosesTheLane(t *testing.T) {
	chain, st, pub := newFakeChain(), newMemStore(), &results{}
	m := newTestManager(t, chain, st, pub)
	canceler := &nopCanceler{}
	m.cfg.Canceler = canceler
	ctx := context.Background()
	_ = m.ExecuteInventory(ctx, request(protocol.InventorySplit, "30"))
	if err := m.ExecuteInventory(ctx, request(protocol.InventoryMerge, "")); err != nil {
		t.Fatal(err)
	}
	if r := pub.last(); r.Status != protocol.ResultSucceeded || r.Shares != 30 || r.RemainingShares != 0 || canceler.calls.Load() != 1 {
		t.Fatalf("merge: %+v (cancels %d)", r, canceler.calls.Load())
	}
	_ = m.ExecuteInventory(ctx, request(protocol.InventorySplit, "30"))
	if r := pub.last(); r.ReasonCode != protocol.ReasonInventoryClosed {
		t.Fatalf("split after merge: %+v", r)
	}
}

func TestAwaitInventoryWaitsOnlyWhenShortWithASplitInFlight(t *testing.T) {
	st := newMemStore()
	m := newTestManager(t, newFakeChain(), st, &results{})
	ctx := context.Background()
	st.held = 50
	start := time.Now()
	if level, _ := m.AwaitInventory(ctx, "lane", "c", "40", start.Add(time.Second)); level.Remaining != f(50) || time.Since(start) > 100*time.Millisecond {
		t.Fatal("waited although the inventory covered the open")
	}
	if _, _ = m.AwaitInventory(ctx, "lane", "c", "80", start.Add(time.Second)); time.Since(start) > 100*time.Millisecond {
		t.Fatal("waited with nothing in flight")
	}
	st.ops["op"] = &store.CTFOperationRecord{OpID: "op", Action: store.CTFSplit, Shares: "50", State: store.CTFSubmitted, Tokens: []string{"up", "down"}}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = m.confirm(ctx, *st.ops["op"], "")
	}()
	level, _ := m.AwaitInventory(ctx, "lane", "c", "80", time.Now().Add(5*time.Second))
	if level.Remaining != f(100) {
		t.Fatalf("woke with %s remaining, want 100", level.Remaining)
	}
}

func TestBookingFailureAfterConfirmKeepsResolving(t *testing.T) {
	chain, st, pub := newFakeChain(), newMemStore(), &results{}
	st.confirmFails = 1
	m := newTestManager(t, chain, st, pub)
	if err := m.ExecuteInventory(context.Background(), request(protocol.InventorySplit, "10")); err == nil {
		t.Fatal("split reported success although booking failed")
	}
	if err := m.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := st.state(st.order[0]); got != store.CTFConfirmed || chain.submitted != 1 {
		t.Fatalf("state %s after %d submissions, want CONFIRMED after 1", got, chain.submitted)
	}
}

func TestSplitRefreshesCollateralAndDropsCachedCash(t *testing.T) {
	chain, st, pub := newFakeChain(), newMemStore(), &results{}
	m := newTestManager(t, chain, st, pub)
	var booked atomic.Int32
	m.cfg.OnBooked = func() { booked.Add(1) }
	if err := m.ExecuteInventory(context.Background(), request(protocol.InventorySplit, "10")); err != nil {
		t.Fatal(err)
	}
	chain.mu.Lock()
	refreshed := append([]string{}, chain.refreshed...)
	chain.mu.Unlock()
	collateral := false
	for _, asset := range refreshed {
		collateral = collateral || asset == "COLLATERAL"
	}
	if !collateral || booked.Load() != 2 {
		t.Fatalf("refreshed %v, OnBooked ran %d times; want COLLATERAL and twice (on booking and after the refresh)", refreshed, booked.Load())
	}
}

func TestUnresolvedBlindOperationHoldsTheCondition(t *testing.T) {
	chain, st, pub := newFakeChain(), newMemStore(), &results{}
	chain.submitErr, chain.lands = clobclient.ErrSubmitOutcomeUnknown, false
	m := newTestManager(t, chain, st, pub)
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	m.cfg.Now = func() time.Time { return time.Unix(0, now.Load()) }
	_ = m.ExecuteInventory(context.Background(), request(protocol.InventorySplit, "10"))
	if r := pub.last(); r.ReasonCode != protocol.ReasonRelayerUnknown {
		t.Fatalf("first split: %+v", r)
	}
	// Another split on the condition would move the balances the first is
	// read from: it is refused, not submitted.
	chain.mu.Lock()
	chain.submitErr = nil
	chain.mu.Unlock()
	_ = m.ExecuteInventory(context.Background(), request(protocol.InventorySplit, "20"))
	if r := pub.last(); r.ReasonCode != protocol.ReasonRelayerUnknown || chain.submitted != 1 {
		t.Fatalf("second split: %+v after %d submissions", r, chain.submitted)
	}
	// Past the deadline the first fails, and the condition is free again.
	now.Store(time.Now().Add(time.Hour).UnixNano())
	if err := m.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.ExecuteInventory(context.Background(), request(protocol.InventorySplit, "20")); err != nil {
		t.Fatal(err)
	}
	if r := pub.last(); r.Status != protocol.ResultSucceeded || chain.submitted != 2 {
		t.Fatalf("split after resolution: %+v after %d submissions", r, chain.submitted)
	}
}

// An operation the relayer cannot report on is not left in flight forever:
// once its signed deadline has passed, the wallet's balances decide it.
// A relayer that cannot be read while a request waits on it is reported,
// once per distinct error rather than every poll.
func TestConfirmWaitReportsRelayerReadErrors(t *testing.T) {
	chain, st, pub := newFakeChain(), newMemStore(), &results{}
	chain.statusErr = errors.New("relayer: 401 unauthorized")
	m := newTestManager(t, chain, st, pub)
	m.cfg.ConfirmWait = 20 * time.Millisecond
	var mu sync.Mutex
	var reported []string
	m.cfg.OnError = func(err error) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, err.Error())
	}
	_ = m.ExecuteInventory(context.Background(), request(protocol.InventorySplit, "10"))
	m.Stop()
	_ = m.Wait(context.Background())
	if r := pub.last(); r.ReasonCode != protocol.ReasonRelayerUnknown {
		t.Fatalf("result %+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	waits := 0
	for _, msg := range reported {
		if strings.HasPrefix(msg, "read relayer transaction") {
			waits++
		}
	}
	if waits != 1 {
		t.Fatalf("reported %d relayer read errors while waiting, want 1: %q", waits, reported)
	}
}

func TestUnreadableRelayerResolvesFromTheChainAfterTheDeadline(t *testing.T) {
	for _, landed := range []bool{true, false} {
		chain, st := newFakeChain(), newMemStore()
		chain.statusErr = errors.New("relayer: 404 not found")
		m := newTestManager(t, chain, st, &results{})
		now := time.Now()
		m.cfg.Now = func() time.Time { return now }
		op := store.CTFOperationRecord{OpID: "op", Action: store.CTFSplit, Shares: "5", State: store.CTFSubmitted,
			RelayerTxID: "tx", Tokens: []string{"up", "down"}, Baseline: []string{"0", "0"}, Deadline: now.Add(time.Minute)}
		st.ops["op"] = &op
		if landed {
			chain.balances["up"].SetInt64(5_000_000)
			chain.balances["down"].SetInt64(5_000_000)
		}
		// Before the deadline the read error is all it reports.
		if done, err := m.resolveOnce(context.Background(), op); done || err == nil {
			t.Fatalf("landed=%v: before the deadline done=%v err=%v", landed, done, err)
		}
		now = now.Add(time.Minute + resolveGrace + time.Second)
		if done, err := m.resolveOnce(context.Background(), op); !done || err != nil {
			t.Fatalf("landed=%v: after the deadline done=%v err=%v", landed, done, err)
		}
		want := store.CTFFailed
		if landed {
			want = store.CTFConfirmed
		}
		if got := st.state("op"); got != want {
			t.Fatalf("landed=%v: state %s, want %s", landed, got, want)
		}
	}
}

// Shutdown stops a resolution still waiting; the operation stays recorded
// for the next start.
func TestStopEndsBackgroundResolutions(t *testing.T) {
	chain, st := newFakeChain(), newMemStore()
	chain.statusErr = errors.New("relayer down")
	m := newTestManager(t, chain, st, &results{})
	m.cfg.Poll = time.Hour
	op := store.CTFOperationRecord{OpID: "op", Action: store.CTFSplit, Shares: "5", State: store.CTFSubmitted,
		RelayerTxID: "tx", Tokens: []string{"up", "down"}, Deadline: time.Now().Add(time.Hour)}
	st.ops["op"] = &op
	m.resolveLater(context.Background(), op)
	m.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Wait(ctx); err != nil {
		t.Fatalf("wait after stop: %v", err)
	}
	if got := st.state("op"); got != store.CTFSubmitted {
		t.Fatalf("a stopped resolution changed the operation to %s", got)
	}
}
