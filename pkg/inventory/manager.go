// Package inventory keeps the split inventory SPLIT_PRESERVE opens sell
// from: it splits pUSD into outcome-token pairs and merges them back through
// the Polymarket relayer, books each operation once it lands, and lets an
// open wait for a split still in flight.
//
// A split tops a lane up to a target rather than adding a fixed amount, so a
// resent request splits nothing and a later one refills what entries used.
// Every operation is recorded before it is submitted and never submitted
// twice: one whose outcome is unknown is resolved from the relayer when it
// has a transaction ID, otherwise from whether the wallet's balance of both
// tokens moved, and failed once its signed deadline has passed without
// landing.
package inventory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// Chain is the wallet, relayer and market access the manager needs; the
// root package's *clobclient.Client implements it.
type Chain interface {
	Market(ctx context.Context, conditionID string) (*clobclient.Market, error)
	SplitCalls(ctx context.Context, conditionID string, amount *big.Int) ([]clobclient.WalletCall, *clobclient.Market, error)
	MergeCalls(ctx context.Context, conditionID string, amount *big.Int) ([]clobclient.WalletCall, *clobclient.Market, error)
	ExecuteWalletBatch(ctx context.Context, calls []clobclient.WalletCall, metadata string, deadline time.Time) (*clobclient.RelayerTransaction, error)
	RelayerTransactionStatus(ctx context.Context, id string) (*clobclient.RelayerTransaction, error)
	TokenBalance(ctx context.Context, tokenID string) (string, error)
	UpdateBalanceAllowance(ctx context.Context, assetType, tokenID string) error
}

// Canceler withdraws the preserves resting against a lane's inventory.
type Canceler interface {
	CancelInventoryOrders(ctx context.Context, uniqueTag, conditionID string, tokens []string) error
}

// Gate suspends a strategy's new exposure (equity drawdown limits).
type Gate interface {
	Suspended(strategy string) (bool, string)
}

type Config struct {
	Chain    Chain
	Store    store.InventoryStore
	Canceler Canceler
	Publish  protocol.ExecutionEventPublisher
	// ClaimCash holds up to want USD of free pUSD for a split and returns
	// what it holds, with a release the manager calls once the split is
	// recorded (and so counted as in flight). Entries size from the same
	// free cash, so the two never spend the same dollars. nil leaves the
	// split to the wallet's balance.
	ClaimCash func(ctx context.Context, want float64) (float64, func(), error)
	Gate      Gate
	// OnBooked runs as soon as an operation books, when the store stops
	// counting a split as in flight, and again once the exchange has been
	// told the wallet's balances changed: the place to drop cached cash.
	OnBooked func()
	Now      func() time.Time
	OnError  func(error)
	// ConfirmWait is how long a request waits for the relayer to confirm
	// before reporting RELAYER_UNKNOWN and resolving in the background.
	ConfirmWait time.Duration
	// BatchTTL is the signed batch's deadline: after it the batch can no
	// longer execute.
	BatchTTL time.Duration
	// Poll is how often a background resolution reads the relayer and chain.
	Poll time.Duration
	// MergeSettle bounds how long a merge waits for cancelled preserves to
	// release their reservations.
	MergeSettle time.Duration
}

type Manager struct {
	cfg Config

	// walletMu serializes submissions: each batch is signed at the wallet's
	// next nonce, so two at once would race for the same one.
	walletMu sync.Mutex

	// splitMu holds a split from reading free cash until its operation is
	// recorded, so the next split's read counts it as in flight.
	splitMu sync.Mutex

	marketsMu sync.Mutex
	markets   map[string][]clobclient.Token

	// booked is closed and replaced whenever an operation books, waking opens
	// waiting for inventory.
	bookedMu sync.Mutex
	booked   chan struct{}

	resolving sync.Map // op ID -> struct{}
	wg        sync.WaitGroup
	// stopped ends background resolutions at shutdown (Stop); Recover
	// resumes them on the next start.
	stopped context.Context
	stop    context.CancelFunc

	// blind counts, per condition, operations resolving with no relayer
	// transaction ID: they are read from the wallet's token balances, which
	// another split or merge on the condition would also move, so none is
	// submitted there until they resolve.
	blindMu sync.Mutex
	blind   map[string]int
}

func New(cfg Config) (*Manager, error) {
	if cfg.Chain == nil || cfg.Store == nil || cfg.Canceler == nil {
		return nil, fmt.Errorf("inventory needs a chain, a store and a canceler")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ConfirmWait <= 0 {
		cfg.ConfirmWait = 3 * time.Minute
	}
	if cfg.BatchTTL <= 0 {
		cfg.BatchTTL = 10 * time.Minute
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 2 * time.Second
	}
	if cfg.MergeSettle <= 0 {
		cfg.MergeSettle = 15 * time.Second
	}
	stopped, stop := context.WithCancel(context.Background())
	return &Manager{cfg: cfg, markets: map[string][]clobclient.Token{}, booked: make(chan struct{}), blind: map[string]int{},
		stopped: stopped, stop: stop}, nil
}

// Stop ends background resolutions; Wait then returns once they have. An
// operation left unresolved stays recorded, and Recover resumes it.
func (m *Manager) Stop() { m.stop() }

// Wait blocks until background resolutions have finished or ctx is done.
func (m *Manager) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Recover resumes every operation a previous process left unfinished.
func (m *Manager) Recover(ctx context.Context) error {
	ops, err := m.cfg.Store.OpenCTFOperations(ctx)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.State == store.CTFPending && len(op.Baseline) == 0 {
			// Never armed, so never submitted.
			m.mark(ctx, op.OpID, store.CTFFailed, "", "", "abandoned before submission")
			continue
		}
		m.resolveLater(ctx, op)
	}
	return nil
}

// ExecuteInventory works one inventory request and publishes its result. It
// returns an error only for a fault, not for a refusal the result reports.
func (m *Manager) ExecuteInventory(ctx context.Context, req protocol.ExecutionInventoryRequest) error {
	result := protocol.ExecutionInventoryResult{
		RequestID: req.RequestID, UniqueTag: req.UniqueTag, Strategy: req.Strategy,
		ConditionID: req.ConditionID, Action: req.Action, Status: protocol.ResultSucceeded,
	}
	err := m.execute(ctx, req, &result)
	if err != nil {
		var refused refusal
		if errors.As(err, &refused) {
			// A refusal is the request's outcome, told in the result; it is
			// no fault of the handler's.
			result.Status, result.ReasonCode, result.Reason = protocol.ResultFailed, refused.code, refused.reason
			err = nil
		} else {
			result.Status, result.ReasonCode, result.Reason = protocol.ResultFailed, protocol.ReasonExecutionFailed, err.Error()
		}
	}
	if req.UniqueTag != "" && req.ConditionID != "" {
		if level, levelErr := m.cfg.Store.InventoryLevel(ctx, req.UniqueTag, req.ConditionID); levelErr == nil {
			result.RemainingShares = toFloat(level.Remaining)
		}
	}
	result.OccurredAt = m.cfg.Now()
	if publishErr := protocol.PublishExecutionInventoryResult(m.cfg.Publish, result); publishErr != nil {
		err = errors.Join(err, fmt.Errorf("publish inventory result: %w", publishErr))
	}
	return err
}

type refusal struct{ code, reason string }

func (r refusal) Error() string { return r.code + ": " + r.reason }

func refuse(code, format string, args ...any) refusal {
	return refusal{code: code, reason: fmt.Sprintf(format, args...)}
}

func (m *Manager) execute(ctx context.Context, req protocol.ExecutionInventoryRequest, result *protocol.ExecutionInventoryResult) error {
	if req.SchemaVersion != protocol.SchemaVersionV1 {
		return refuse(protocol.ReasonInvalidIntent, "unsupported schema version %q", req.SchemaVersion)
	}
	if strings.TrimSpace(req.UniqueTag) == "" || req.Strategy == "" || req.ConditionID == "" {
		return refuse(protocol.ReasonInvalidIntent, "unique tag, strategy and condition ID are required")
	}
	if protocol.IsInventoryTag(req.UniqueTag) {
		return refuse(protocol.ReasonInvalidIntent, "unique tag %q names an inventory lane; name the strategy lane", req.UniqueTag)
	}
	if !req.ExpiresAt.IsZero() && !req.ExpiresAt.After(m.cfg.Now()) {
		return refuse(protocol.ReasonExpired, "request expired at %s", req.ExpiresAt.UTC().Format(time.RFC3339Nano))
	}
	switch req.Action {
	case protocol.InventorySplit:
		return m.split(ctx, req, result)
	case protocol.InventoryMerge:
		return m.merge(ctx, req, result)
	default:
		return refuse(protocol.ReasonInvalidIntent, "unknown inventory action %q", req.Action)
	}
}

func (m *Manager) split(ctx context.Context, req protocol.ExecutionInventoryRequest, result *protocol.ExecutionInventoryResult) error {
	if !decimal.Positive(req.TargetShares) {
		return refuse(protocol.ReasonInvalidIntent, "a split needs positive target_shares, got %q", req.TargetShares)
	}
	if m.cfg.Gate != nil {
		if suspended, reason := m.cfg.Gate.Suspended(req.Strategy); suspended {
			return refuse(protocol.ReasonDrawdownLimit, "%s", reason)
		}
	}
	tokens, err := m.tokens(ctx, req.ConditionID)
	if err != nil {
		return err
	}
	maxShares, release := "", func() {}
	m.splitMu.Lock()
	if m.cfg.ClaimCash != nil {
		// A pair costs one dollar, and the gap is at most the target.
		want, err := decimal.NonNegativeFloat(req.TargetShares)
		if err != nil {
			m.splitMu.Unlock()
			return refuse(protocol.ReasonInvalidIntent, "target_shares %q: %s", req.TargetShares, err)
		}
		free, claimed, err := m.cfg.ClaimCash(ctx, want)
		if err != nil {
			m.splitMu.Unlock()
			return fmt.Errorf("claim free cash: %w", err)
		}
		release = claimed
		if free < 0 {
			free = 0
		}
		maxShares = fmt.Sprintf("%.6f", free-0.0000005)
		if free < 0.000001 {
			maxShares = "0"
		}
	}
	op, gap, ok, err := m.cfg.Store.BeginSplit(ctx, m.operation(req, tokens), maxShares)
	// The split is recorded, and InFlightSplitCash counts it from here, or
	// it failed: either way the claim's work is done.
	release()
	m.splitMu.Unlock()
	if errors.Is(err, store.ErrInventoryClosed) {
		return refuse(protocol.ReasonInventoryClosed, "lane %s has merged its inventory", req.UniqueTag)
	}
	if err != nil {
		return err
	}
	capped := decimal.Positive(gap) && (!ok || decimal.Compare(op.Shares, gap) < 0)
	if !ok {
		if capped {
			return refuse(protocol.ReasonInventoryCapped, "no free cash to split the %s shares short of target", gap)
		}
		return nil
	}
	if err := m.run(ctx, op, result); err != nil {
		return err
	}
	if capped {
		result.ReasonCode = protocol.ReasonInventoryCapped
		result.Reason = fmt.Sprintf("free cash split %s of the %s shares short of target", op.Shares, gap)
	}
	return nil
}

func (m *Manager) merge(ctx context.Context, req protocol.ExecutionInventoryRequest, result *protocol.ExecutionInventoryResult) error {
	if req.Shares != "" && !decimal.Positive(req.Shares) {
		return refuse(protocol.ReasonInvalidIntent, "merge shares must be positive, got %q", req.Shares)
	}
	tokens, err := m.tokens(ctx, req.ConditionID)
	if err != nil {
		return err
	}
	op := m.operation(req, tokens)
	// Close the lane before cancelling, so no new preserve can reserve the
	// pairs the cancels free.
	op, err = m.cfg.Store.CloseInventory(ctx, op)
	if err != nil {
		return err
	}
	ids := []string{tokens[0].TokenID, tokens[1].TokenID}
	if err := m.cfg.Canceler.CancelInventoryOrders(ctx, req.UniqueTag, req.ConditionID, ids); err != nil {
		m.report(fmt.Errorf("cancel preserves before merging %s: %w", req.UniqueTag, err))
	}
	// A cancelled preserve frees its pairs once its order is terminal; what
	// is still held when the wait runs out stays unmerged and settles with
	// the market.
	deadline := m.cfg.Now().Add(m.cfg.MergeSettle)
	for {
		level, err := m.cfg.Store.InventoryLevel(ctx, req.UniqueTag, req.ConditionID)
		if err != nil {
			return err
		}
		if !decimal.Positive(level.Reserved) || !m.cfg.Now().Before(deadline) {
			break
		}
		if err := sleep(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
	op.Shares = req.Shares
	op, ok, err := m.cfg.Store.BeginMerge(ctx, op)
	if err != nil || !ok {
		return err
	}
	return m.run(ctx, op, result)
}

func (m *Manager) operation(req protocol.ExecutionInventoryRequest, tokens []clobclient.Token) store.CTFOperationRecord {
	return store.CTFOperationRecord{
		OpID: newID(), RequestID: req.RequestID, Strategy: req.Strategy, UniqueTag: req.UniqueTag,
		MarketID: req.MarketID, ConditionID: req.ConditionID, TargetShares: req.TargetShares,
		Tokens:   []string{tokens[0].TokenID, tokens[1].TokenID},
		Outcomes: []string{tokens[0].Outcome, tokens[1].Outcome},
	}
}

// run submits a recorded operation and books it once it lands. It returns
// with RELAYER_UNKNOWN when the outcome is not known within ConfirmWait,
// leaving the operation to a background resolution.
func (m *Manager) run(ctx context.Context, op store.CTFOperationRecord, result *protocol.ExecutionInventoryResult) error {
	m.walletMu.Lock()
	defer m.walletMu.Unlock()
	if m.blindOn(op.ConditionID) {
		m.mark(ctx, op.OpID, store.CTFFailed, "", "", "an earlier operation on the condition is unresolved")
		return refuse(protocol.ReasonRelayerUnknown, "an earlier split or merge on %s is still being resolved from the chain; retry once it settles", op.ConditionID)
	}
	amount, err := clobclient.ParseUnits(op.Shares)
	if err != nil {
		m.mark(ctx, op.OpID, store.CTFFailed, "", "", err.Error())
		return err
	}
	build := m.cfg.Chain.SplitCalls
	if op.Action == store.CTFMerge {
		build = m.cfg.Chain.MergeCalls
	}
	calls, _, err := build(ctx, op.ConditionID, amount)
	if err != nil {
		m.mark(ctx, op.OpID, store.CTFFailed, "", "", err.Error())
		if op.Action == store.CTFSplit && (strings.Contains(err.Error(), "is closed") || strings.Contains(err.Error(), "is resolved")) {
			return refuse(protocol.ReasonInventoryClosed, "%s", err)
		}
		return refuse(protocol.ReasonRelayerFailed, "build %s: %s", strings.ToLower(string(op.Action)), err)
	}
	baseline, err := m.balances(ctx, op.Tokens)
	if err != nil {
		m.mark(ctx, op.OpID, store.CTFFailed, "", "", err.Error())
		return refuse(protocol.ReasonRelayerFailed, "read balances before %s: %s", strings.ToLower(string(op.Action)), err)
	}
	now := m.cfg.Now()
	op.Baseline, op.ArmedAt, op.Deadline = baseline, now, now.Add(m.cfg.BatchTTL)
	if err := m.cfg.Store.ArmCTFOperation(ctx, op.OpID, op.Baseline, op.ArmedAt, op.Deadline); err != nil {
		m.mark(ctx, op.OpID, store.CTFFailed, "", "", err.Error())
		return err
	}
	metadata := fmt.Sprintf("executiond %s %s %s", strings.ToLower(string(op.Action)), op.Shares, op.ConditionID)
	tx, err := m.cfg.Chain.ExecuteWalletBatch(ctx, calls, metadata, op.Deadline)
	switch {
	case errors.Is(err, clobclient.ErrSubmitOutcomeUnknown):
		m.mark(ctx, op.OpID, store.CTFUnknown, "", "", err.Error())
		m.resolveLater(ctx, op)
		return refuse(protocol.ReasonRelayerUnknown, "submission outcome unknown; resolving from the chain: %s", err)
	case err != nil:
		m.mark(ctx, op.OpID, store.CTFFailed, "", "", err.Error())
		return refuse(protocol.ReasonRelayerFailed, "%s", err)
	}
	op.RelayerTxID, op.State = tx.ID, store.CTFSubmitted
	m.mark(ctx, op.OpID, store.CTFSubmitted, tx.ID, tx.Hash, "")
	waitCtx, cancel := context.WithTimeout(ctx, m.cfg.ConfirmWait)
	defer cancel()
	lastErr := ""
	for {
		status, err := m.cfg.Chain.RelayerTransactionStatus(waitCtx, tx.ID)
		if err != nil && waitCtx.Err() == nil && err.Error() != lastErr {
			// Reported once each time it changes, not every poll.
			lastErr = err.Error()
			m.report(fmt.Errorf("read relayer transaction %s: %w", tx.ID, err))
		}
		if err == nil {
			if status.State == "STATE_CONFIRMED" {
				if err := m.confirm(ctx, op, status.Hash); err != nil {
					// It landed; keep trying to book it rather than leave it
					// SUBMITTED until a restart.
					m.resolveLater(ctx, op)
					return err
				}
				result.Shares, result.TxHash = toFloat(op.Shares), status.Hash
				return nil
			}
			if status.Failed() {
				m.mark(ctx, op.OpID, store.CTFFailed, "", status.Hash, status.State+": "+status.Error)
				return refuse(protocol.ReasonRelayerFailed, "relayer transaction %s ended %s: %s", tx.ID, status.State, status.Error)
			}
		}
		if sleep(waitCtx, m.cfg.Poll) != nil {
			m.resolveLater(ctx, op)
			return refuse(protocol.ReasonRelayerUnknown, "relayer transaction %s not confirmed within %s; resolving in the background", tx.ID, m.cfg.ConfirmWait)
		}
	}
}

// confirm books a landed operation and tells the exchange the wallet's
// token balances changed, so a preserve's sell is accepted at once.
func (m *Manager) confirm(ctx context.Context, op store.CTFOperationRecord, txHash string) error {
	if err := m.cfg.Store.ConfirmCTFOperation(ctx, op.OpID, txHash, m.cfg.Now().UTC()); err != nil {
		return fmt.Errorf("book %s %s: %w", strings.ToLower(string(op.Action)), op.OpID, err)
	}
	// Booking drops a split from InFlightSplitCash: cached cash from before
	// it landed would now read the spent pUSD as free.
	if m.cfg.OnBooked != nil {
		m.cfg.OnBooked()
	}
	for _, token := range op.Tokens {
		if err := m.cfg.Chain.UpdateBalanceAllowance(ctx, "CONDITIONAL", token); err != nil {
			m.report(fmt.Errorf("refresh exchange balance of %s: %w", token, err))
		}
	}
	// A split spends pUSD and a merge returns it: either way free cash and
	// equity must read the new balance.
	if err := m.cfg.Chain.UpdateBalanceAllowance(ctx, "COLLATERAL", ""); err != nil {
		m.report(fmt.Errorf("refresh exchange collateral balance: %w", err))
	}
	if m.cfg.OnBooked != nil {
		m.cfg.OnBooked()
	}
	m.bookedMu.Lock()
	close(m.booked)
	m.booked = make(chan struct{})
	m.bookedMu.Unlock()
	return nil
}

// resolveLater settles an operation whose outcome is not yet known: from the
// relayer alone when it has a transaction ID, otherwise from whether the
// wallet's balance of both tokens moved by its shares since it was armed,
// holding off other operations on the condition meanwhile. One that never
// lands is failed once its signed deadline has passed.
func (m *Manager) resolveLater(ctx context.Context, op store.CTFOperationRecord) {
	if _, running := m.resolving.LoadOrStore(op.OpID, struct{}{}); running {
		return
	}
	blind := op.RelayerTxID == ""
	if blind {
		m.blindMu.Lock()
		m.blind[op.ConditionID]++
		m.blindMu.Unlock()
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.resolving.Delete(op.OpID)
		if blind {
			defer func() {
				m.blindMu.Lock()
				if m.blind[op.ConditionID]--; m.blind[op.ConditionID] <= 0 {
					delete(m.blind, op.ConditionID)
				}
				m.blindMu.Unlock()
			}()
		}
		// A resolution outlives the request that started it, but not the
		// process: shutdown stops it, and the next start resumes it.
		ctx := m.stopped
		for {
			done, err := m.resolveOnce(ctx, op)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				m.report(fmt.Errorf("resolve %s %s: %w", strings.ToLower(string(op.Action)), op.OpID, err))
			}
			if done || sleep(ctx, m.cfg.Poll) != nil {
				return
			}
		}
	}()
}

func (m *Manager) resolveOnce(ctx context.Context, op store.CTFOperationRecord) (bool, error) {
	if op.RelayerTxID != "" {
		// The relayer knows this transaction; the wallet's balances, which
		// trades and other operations also move, are no evidence beside it.
		status, err := m.cfg.Chain.RelayerTransactionStatus(ctx, op.RelayerTxID)
		if err != nil {
			if !m.pastDeadline(op) {
				return false, err
			}
			// The relayer cannot be read, but the signed batch can no
			// longer execute, so whatever it did is on chain now: the
			// wallet's balances decide rather than leave it in flight
			// forever, short-changing every refill and free-cash read.
			landed, landedErr := m.landed(ctx, op)
			if landedErr != nil {
				return false, errors.Join(err, landedErr)
			}
			if landed {
				if err := m.confirm(ctx, op, ""); err != nil {
					return false, err
				}
				return true, nil
			}
			m.mark(ctx, op.OpID, store.CTFFailed, "", "", "signed deadline passed, the relayer cannot be read and the chain shows no change: "+err.Error())
			return true, nil
		}
		if status.State == "STATE_CONFIRMED" {
			if err := m.confirm(ctx, op, status.Hash); err != nil {
				return false, err
			}
			return true, nil
		}
		if status.Failed() {
			m.mark(ctx, op.OpID, store.CTFFailed, "", status.Hash, status.State+": "+status.Error)
			return true, nil
		}
		if m.pastDeadline(op) {
			// The deadline only stops a batch not yet executed; one the
			// relayer saw executed or mined is on chain, so the wallet's
			// balances decide.
			landed, err := m.landed(ctx, op)
			if err != nil {
				return false, err
			}
			if landed {
				if err := m.confirm(ctx, op, status.Hash); err != nil {
					return false, err
				}
				return true, nil
			}
			m.mark(ctx, op.OpID, store.CTFFailed, "", status.Hash, "signed deadline passed, the relayer still reports "+status.State+" and the chain shows no change")
			return true, nil
		}
		return false, nil
	}
	landed, err := m.landed(ctx, op)
	if err != nil {
		return false, err
	}
	if landed {
		if err := m.confirm(ctx, op, ""); err != nil {
			return false, err
		}
		return true, nil
	}
	if m.pastDeadline(op) {
		m.mark(ctx, op.OpID, store.CTFFailed, "", "", "signed deadline passed and the chain shows no change")
		return true, nil
	}
	return false, nil
}

// landed reports whether the wallet's balance of every token moved by the
// operation's shares since it was armed, up for a split and down for a
// merge, once the wallet's own exchange fills since then are taken out:
// other lanes keep trading the condition's tokens while it is unresolved.
//
// A fill not yet recorded, or one matched before the baseline and mined
// after it, still skews a read; a split or merge moves both tokens by its
// shares at once, which a stray trade on one token does not imitate.
func (m *Manager) landed(ctx context.Context, op store.CTFOperationRecord) (bool, error) {
	if len(op.Baseline) != len(op.Tokens) {
		return false, nil
	}
	amount, err := clobclient.ParseUnits(op.Shares)
	if err != nil {
		return false, err
	}
	now, err := m.balances(ctx, op.Tokens)
	if err != nil {
		return false, err
	}
	since := op.ArmedAt
	if since.IsZero() {
		// Armed before armed_at was recorded: the deadline less the batch
		// lifetime is when it was armed, unless BatchTTL has since changed.
		since = op.Deadline.Add(-m.cfg.BatchTTL)
	}
	traded, err := m.cfg.Store.ExchangeNetShares(ctx, op.Tokens, since)
	if err != nil {
		return false, err
	}
	for i := range op.Tokens {
		before, ok1 := new(big.Int).SetString(op.Baseline[i], 10)
		after, ok2 := new(big.Int).SetString(now[i], 10)
		net, ok3 := new(big.Int).SetString(traded[i], 10)
		if !ok1 || !ok2 || !ok3 {
			return false, fmt.Errorf("unreadable balance or fills of %s", op.Tokens[i])
		}
		moved := new(big.Int).Sub(after, before)
		moved.Sub(moved, net)
		if op.Action == store.CTFMerge {
			moved.Neg(moved)
		}
		if moved.Cmp(amount) < 0 {
			return false, nil
		}
	}
	return true, nil
}

// resolveGrace is how long after its signed deadline an operation may still
// be seen landing: a block mined at the deadline, read late.
const resolveGrace = time.Minute

// pastDeadline reports whether an operation's signed batch can no longer
// execute, with resolveGrace for it to be seen.
func (m *Manager) pastDeadline(op store.CTFOperationRecord) bool {
	return !op.Deadline.IsZero() && m.cfg.Now().After(op.Deadline.Add(resolveGrace))
}

func (m *Manager) blindOn(conditionID string) bool {
	m.blindMu.Lock()
	defer m.blindMu.Unlock()
	return m.blind[conditionID] > 0
}

func (m *Manager) balances(ctx context.Context, tokens []string) ([]string, error) {
	out := make([]string, len(tokens))
	for i, token := range tokens {
		balance, err := m.cfg.Chain.TokenBalance(ctx, token)
		if err != nil {
			return nil, err
		}
		out[i] = balance
	}
	return out, nil
}

func (m *Manager) mark(ctx context.Context, opID string, state store.CTFOpState, txID, txHash, errText string) {
	if err := m.cfg.Store.MarkCTFOperation(ctx, opID, state, txID, txHash, errText); err != nil {
		m.report(fmt.Errorf("mark %s %s: %w", opID, state, err))
	}
}

// tokens returns a binary condition's two outcome tokens, cached: they never
// change.
func (m *Manager) tokens(ctx context.Context, conditionID string) ([]clobclient.Token, error) {
	m.marketsMu.Lock()
	tokens, ok := m.markets[conditionID]
	m.marketsMu.Unlock()
	if ok {
		return tokens, nil
	}
	market, err := m.cfg.Chain.Market(ctx, conditionID)
	if err != nil {
		return nil, fmt.Errorf("read market %s: %w", conditionID, err)
	}
	if len(market.Tokens) != 2 {
		return nil, refuse(protocol.ReasonInvalidIntent, "market %s has %d outcome tokens, want 2", conditionID, len(market.Tokens))
	}
	tokens = append([]clobclient.Token{}, market.Tokens...)
	m.marketsMu.Lock()
	m.markets[conditionID] = tokens
	m.marketsMu.Unlock()
	return tokens, nil
}

// Complement names the other outcome token of a binary condition.
func (m *Manager) Complement(ctx context.Context, conditionID, tokenID string) (string, string, error) {
	tokens, err := m.tokens(ctx, conditionID)
	if err != nil {
		return "", "", err
	}
	switch tokenID {
	case tokens[0].TokenID:
		return tokens[1].TokenID, tokens[1].Outcome, nil
	case tokens[1].TokenID:
		return tokens[0].TokenID, tokens[0].Outcome, nil
	}
	return "", "", fmt.Errorf("token %s is not an outcome of %s", tokenID, conditionID)
}

// AwaitInventory reads a lane's inventory, waiting while it is short of
// shares and a split is still in flight, until deadline. It never waits on a
// lane with enough inventory, or with nothing in flight to wait for. With the
// level comes the inventory lane's position in sellToken, read in the same
// round trip, so the sell's planning need not read it again.
func (m *Manager) AwaitInventory(ctx context.Context, uniqueTag, conditionID, sellToken, shares string, deadline time.Time) (store.InventoryLevel, store.PositionRecord, bool, error) {
	for {
		m.bookedMu.Lock()
		booked := m.booked
		m.bookedMu.Unlock()
		level, position, found, err := m.cfg.Store.InventoryLevelAndPosition(ctx, uniqueTag, conditionID, sellToken)
		if err != nil {
			return level, position, found, err
		}
		now := m.cfg.Now()
		if decimal.Compare(level.Remaining, shares) >= 0 || !decimal.Positive(level.InFlight) || deadline.IsZero() || !now.Before(deadline) {
			return level, position, found, nil
		}
		wait := 250 * time.Millisecond
		if left := deadline.Sub(now); left < wait {
			wait = left
		}
		select {
		case <-ctx.Done():
			return level, position, found, ctx.Err()
		case <-booked:
		case <-time.After(wait):
		}
	}
}

func (m *Manager) report(err error) {
	if m.cfg.OnError != nil {
		m.cfg.OnError(err)
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func toFloat(value string) float64 {
	f, err := decimal.NonNegativeFloat(value)
	if err != nil {
		return 0
	}
	return f
}

func newID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("ctf-%d", time.Now().UnixNano())
	}
	return "ctf-" + hex.EncodeToString(raw[:])
}
