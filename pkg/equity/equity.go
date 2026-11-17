// Package equity values the trading wallet: its USDC cash plus the recorded
// positions marked at the best bid. It is the base that equity-fraction sizing
// multiplies against.
package equity

import (
	"context"
	"fmt"
	"sync"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/settlement"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// DefaultCashTTL is how long a collateral balance read is reused. Sizing may
// run for many intents a second; the exchange balance moves only on fills.
const DefaultCashTTL = 5 * time.Second

// DefaultFillSettle is how long after a fill a balance read is not cached:
// the exchange balance can lag the match, so a read that soon may still
// count the filled order's cash and must not be reused for a whole TTL.
const DefaultFillSettle = 2 * time.Second

type BalanceReader interface {
	BalanceAllowance(ctx context.Context, assetType, tokenID string) (*clobclient.BalanceAllowance, error)
}

type QuoteSource interface {
	Get(conditionID string) (marketquotes.Snapshot, bool)
}

// Snapshot is one valuation of the wallet.
type Snapshot struct {
	CashUSD      float64
	PositionsUSD float64
	EquityUSD    float64
	// PositionsByStrategy splits PositionsUSD by the strategy of each lane,
	// lanes with no recorded strategy under "". It is filled only by
	// SnapshotWithStrategies.
	PositionsByStrategy map[string]float64
	// Positions counts the lanes holding shares. SettledPositions counts those
	// among them in resolved markets, valued at $1 per winning share still in
	// the wallet and 0 for a loser; UnmarkedPositions those with neither a
	// usable bid nor a resolution, which are valued at their entry price.
	// LookupFailures counts the unmarked lanes whose market or balance could
	// not be read: they are among UnmarkedPositions, and the failure is what
	// the settlement sweeper reports.
	Positions         int
	SettledPositions  int
	UnmarkedPositions int
	LookupFailures    int
	CashAsOf          time.Time
	AsOf              time.Time
}

type Tracker struct {
	balances  BalanceReader
	positions store.PositionStore
	quotes    QuoteSource
	now       func() time.Time

	cashTTL     time.Duration
	fillSettle  time.Duration
	maxQuoteAge time.Duration
	// settled values positions in markets that have resolved; nil values
	// them like any other unquoted position (SetSettlements).
	settled *settlement.Resolver

	mu     sync.Mutex
	cash   float64
	cashAt time.Time
	// gen counts invalidations, so a read that was in flight when a fill
	// invalidated the cache is not stored as though it followed the fill.
	gen uint64
	// invalidatedAt is when the last fill invalidated the cache.
	invalidatedAt time.Time
	// reading serializes exchange reads, so callers that find the cache
	// stale share one round trip without holding mu across it.
	reading sync.Mutex
	// wake asks RunCashRefresh to read now, after an invalidation.
	wake chan struct{}
}

func New(balances BalanceReader, positions store.PositionStore, quotes QuoteSource, now func() time.Time) (*Tracker, error) {
	if balances == nil || positions == nil || quotes == nil {
		return nil, fmt.Errorf("balance reader, position store, and quote source are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Tracker{balances: balances, positions: positions, quotes: quotes, now: now, cashTTL: DefaultCashTTL, fillSettle: DefaultFillSettle, wake: make(chan struct{}, 1)}, nil
}

// SetCashTTL sets how long a balance read is reused; 0 reads on every call.
func (t *Tracker) SetCashTTL(ttl time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cashTTL = ttl
}

// SetFillSettle sets how long after an invalidation a balance read is used
// but not cached; 0 caches at once.
func (t *Tracker) SetFillSettle(settle time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fillSettle = settle
}

// SetMaxQuoteAge makes a bid older than age count as missing. 0 accepts any age.
func (t *Tracker) SetMaxQuoteAge(age time.Duration) {
	t.maxQuoteAge = age
}

// Invalidate drops the cached balance so the next read goes to the exchange.
func (t *Tracker) Invalidate() {
	t.mu.Lock()
	t.cashAt = time.Time{}
	t.gen++
	t.invalidatedAt = t.now()
	t.mu.Unlock()
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// RunCashRefresh keeps the balance fresh off the order path until ctx ends:
// it reads again at half the TTL, so a cached value never expires under an
// entry, and once the fill settle window has passed after an invalidation,
// so the post-fill balance is cached soon after the exchange shows it. Entries still read the
// exchange themselves when this has not caught up, so the cache is never
// trusted past its TTL or across a fill. onErr, if set, sees failed reads.
func (t *Tracker) RunCashRefresh(ctx context.Context, onErr func(error)) {
	t.mu.Lock()
	ttl := t.cashTTL
	t.mu.Unlock()
	if ttl <= 0 {
		return
	}
	ticker := time.NewTicker(ttl / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-t.wake:
			// A read inside the settle window would not be cached.
			t.mu.Lock()
			wait := t.invalidatedAt.Add(t.fillSettle).Sub(t.now())
			t.mu.Unlock()
			if wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}
		readCtx, cancel := context.WithTimeout(ctx, ttl)
		_, _, err := t.read(readCtx, ttl/2)
		cancel()
		if err != nil && ctx.Err() == nil && onErr != nil {
			onErr(err)
		}
	}
}

// Cash returns the wallet's USDC balance in dollars and when it was read.
// The CLOB does not deduct resting orders from it: a working buy still counts
// as cash until it fills.
func (t *Tracker) Cash(ctx context.Context) (float64, time.Time, error) {
	t.mu.Lock()
	ttl := t.cashTTL
	if cash, at, ok := t.freshLocked(ttl); ok {
		t.mu.Unlock()
		return cash, at, nil
	}
	t.mu.Unlock()
	return t.read(ctx, ttl)
}

// freshLocked returns the cached balance if it is younger than maxAge. t.mu
// must be held.
func (t *Tracker) freshLocked(maxAge time.Duration) (float64, time.Time, bool) {
	if !t.cashAt.IsZero() && t.now().Sub(t.cashAt) < maxAge {
		return t.cash, t.cashAt, true
	}
	return 0, time.Time{}, false
}

// read fetches the balance from the exchange unless one younger than maxAge
// is cached by the time it gets its turn. A read overtaken by an
// invalidation is done again, since it may predate the fill. A read begun
// within the fill settle window is returned but not cached.
func (t *Tracker) read(ctx context.Context, maxAge time.Duration) (float64, time.Time, error) {
	t.reading.Lock()
	defer t.reading.Unlock()
	for attempt := 0; ; attempt++ {
		t.mu.Lock()
		if cash, at, ok := t.freshLocked(maxAge); ok {
			t.mu.Unlock()
			return cash, at, nil
		}
		gen := t.gen
		settling := !t.invalidatedAt.IsZero() && t.now().Sub(t.invalidatedAt) < t.fillSettle
		t.mu.Unlock()
		balance, err := t.balances.BalanceAllowance(ctx, "COLLATERAL", "")
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("read collateral balance: %w", err)
		}
		cash, err := decimal.FromBaseUnits(balance.Balance)
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("collateral balance: %w", err)
		}
		now := t.now()
		t.mu.Lock()
		if t.gen == gen {
			if !settling {
				t.cash, t.cashAt = cash, now
			}
			t.mu.Unlock()
			return cash, now, nil
		}
		t.mu.Unlock()
		if attempt >= 2 {
			// Fills keep overtaking the read: use it, but do not cache a
			// balance that may predate one.
			return cash, now, nil
		}
	}
}

// Snapshot values the wallet now. It fails rather than guess when the cash
// balance cannot be read, since every size derived from it would be wrong.
// It skips the per-strategy split, which the order path does not need.
func (t *Tracker) Snapshot(ctx context.Context) (Snapshot, error) {
	return t.snapshot(ctx, false)
}

// SnapshotWithStrategies is Snapshot with PositionsByStrategy filled. It
// fails when the positions store cannot name lanes' strategies, rather than
// record a split that leaves positions out.
func (t *Tracker) SnapshotWithStrategies(ctx context.Context) (Snapshot, error) {
	return t.snapshot(ctx, true)
}

func (t *Tracker) snapshot(ctx context.Context, withStrategies bool) (Snapshot, error) {
	cash, cashAt, err := t.Cash(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	records, err := t.positions.PositionFeatures(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load positions for equity: %w", err)
	}
	var strategies map[store.LaneKey]string
	if withStrategies {
		reader, ok := t.positions.(store.LaneStrategyReader)
		if !ok {
			return Snapshot{}, fmt.Errorf("positions store cannot name lane strategies")
		}
		if strategies, err = reader.LaneStrategies(ctx); err != nil {
			return Snapshot{}, fmt.Errorf("load lane strategies for equity: %w", err)
		}
	}
	now := t.now()
	snapshot := Snapshot{CashUSD: cash, CashAsOf: cashAt.UTC(), AsOf: now.UTC()}
	if withStrategies {
		snapshot.PositionsByStrategy = map[string]float64{}
	}
	var unquoted []held
	for _, record := range records {
		if !decimal.Positive(record.PositionSize) {
			continue
		}
		shares, err := decimal.PositiveFloat(record.PositionSize)
		if err != nil {
			return Snapshot{}, fmt.Errorf("position %s/%s/%s: %w", record.ConditionID, record.TokenID, record.UniqueTag, err)
		}
		snapshot.Positions++
		if price, marked := t.bid(record.ConditionID, record.TokenID, now); marked {
			snapshot.add(strategies, record, shares*price)
			continue
		}
		unquoted = append(unquoted, held{record: record, shares: shares})
	}
	for i, value := range t.settledValues(ctx, unquoted, now) {
		position := unquoted[i]
		if value.ok {
			snapshot.SettledPositions++
			snapshot.add(strategies, position.record, value.usd)
			continue
		}
		snapshot.UnmarkedPositions++
		if value.failed {
			snapshot.LookupFailures++
		}
		price, err := decimal.NonNegativeFloat(position.record.EntryPrice)
		if err != nil {
			r := position.record
			return Snapshot{}, fmt.Errorf("position %s/%s/%s has no quote and no usable entry price: %w", r.ConditionID, r.TokenID, r.UniqueTag, err)
		}
		snapshot.add(strategies, position.record, position.shares*price)
	}
	snapshot.EquityUSD = snapshot.CashUSD + snapshot.PositionsUSD
	return snapshot, nil
}

func (s *Snapshot) add(strategies map[store.LaneKey]string, position store.PositionRecord, usd float64) {
	s.PositionsUSD += usd
	if strategies != nil {
		s.PositionsByStrategy[strategies[store.LaneKey{ConditionID: position.ConditionID, TokenID: position.TokenID, UniqueTag: position.UniqueTag}]] += usd
	}
}

// settleLookups bounds concurrent CLOB reads for unquoted positions, which
// on the first valuation after a start can be every settled lane at once.
const settleLookups = 8

// SetSettlements lets the tracker value positions in markets that have
// resolved: $1 per winning share the wallet still holds, never more than the
// lanes recorded (redeemed shares are already cash), and nothing for a loser.
// Without it such a position has no quote and is valued at its entry price.
func (t *Tracker) SetSettlements(resolver *settlement.Resolver) { t.settled = resolver }

type held struct {
	record store.PositionRecord
	shares float64
}

type settledValue struct {
	usd    float64
	ok     bool
	failed bool
}

// settledValues values the unquoted positions whose markets have resolved. A
// lane whose lookup fails is valued at its entry price like an unresolved
// one, so one market the CLOB cannot serve does not stop every valuation; the
// resolver holds the failure for a while, so it costs one request per
// interval, and the sweeper reports it. Lanes holding the same winning token
// share its wallet balance, handed out in store order, so no two of them
// count the same shares; a lane that changed within settlement.SettleGrace
// is taken to hold what it recorded, since its tokens may not have landed.
func (t *Tracker) settledValues(ctx context.Context, positions []held, now time.Time) []settledValue {
	values := make([]settledValue, len(positions))
	if t.settled == nil || len(positions) == 0 {
		return values
	}
	outcomes := make([]settlement.Outcome, len(positions))
	failed := make([]bool, len(positions))
	var (
		wg    sync.WaitGroup
		slots = make(chan struct{}, settleLookups)
	)
	for i, position := range positions {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			r := position.record
			outcome, err := t.settled.Outcome(ctx, r.ConditionID, r.TokenID)
			outcomes[i], failed[i] = outcome, err != nil
		}()
	}
	wg.Wait()
	remaining := map[string]float64{}
	for i, position := range positions {
		outcome := outcomes[i]
		switch {
		case failed[i]:
			values[i] = settledValue{failed: true}
		case !outcome.Resolved:
		case !outcome.Winner:
			values[i] = settledValue{ok: true}
		default:
			token := position.record.TokenID
			if _, seen := remaining[token]; !seen {
				remaining[token] = outcome.WalletShares
			}
			usd := position.shares
			if settlement.Settled(position.record, now) {
				usd = min(position.shares, max(remaining[token], 0))
			}
			remaining[token] -= usd
			values[i] = settledValue{usd: usd, ok: true}
		}
	}
	return values
}

// bid is the price a position could be sold at now: the best bid of its token
// from the latest PMM quote. Kelly sizing wants liquidation value, not mid, so
// a fresh quote with no bid marks the position at zero.
func (t *Tracker) bid(conditionID, tokenID string, now time.Time) (float64, bool) {
	snapshot, ok := t.quotes.Get(conditionID)
	if !ok || (t.maxQuoteAge > 0 && now.Sub(snapshot.At) > t.maxQuoteAge) {
		return 0, false
	}
	switch tokenID {
	case snapshot.Up.AssetID:
		return snapshot.Up.Bid, true
	case snapshot.Down.AssetID:
		return snapshot.Down.Bid, true
	}
	return 0, false
}
