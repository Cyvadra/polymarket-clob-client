// Package equity values the trading wallet: its USDC cash plus the recorded
// positions marked at the best bid. It is the base that equity-fraction sizing
// multiplies against.
package equity

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// collateralUnit converts the CLOB's collateral balance, reported in USDC's
// 6-decimal base units, to dollars.
const collateralUnit = 1_000_000

// DefaultCashTTL is how long a collateral balance read is reused. Sizing may
// run for many intents a second; the exchange balance moves only on fills.
const DefaultCashTTL = 5 * time.Second

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
	// Positions counts the lanes holding shares; UnmarkedPositions counts those
	// among them with no usable bid, which are valued at their entry price.
	Positions         int
	UnmarkedPositions int
	CashAsOf          time.Time
	AsOf              time.Time
}

type Tracker struct {
	balances  BalanceReader
	positions store.PositionStore
	quotes    QuoteSource
	now       func() time.Time

	cashTTL     time.Duration
	maxQuoteAge time.Duration

	mu     sync.Mutex
	cash   float64
	cashAt time.Time
}

func New(balances BalanceReader, positions store.PositionStore, quotes QuoteSource, now func() time.Time) (*Tracker, error) {
	if balances == nil || positions == nil || quotes == nil {
		return nil, fmt.Errorf("balance reader, position store, and quote source are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Tracker{balances: balances, positions: positions, quotes: quotes, now: now, cashTTL: DefaultCashTTL}, nil
}

// SetCashTTL sets how long a balance read is reused; 0 reads on every call.
func (t *Tracker) SetCashTTL(ttl time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cashTTL = ttl
}

// SetMaxQuoteAge makes a bid older than age count as missing. 0 accepts any age.
func (t *Tracker) SetMaxQuoteAge(age time.Duration) {
	t.maxQuoteAge = age
}

// Invalidate drops the cached balance so the next read goes to the exchange.
func (t *Tracker) Invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cashAt = time.Time{}
}

// Cash returns the wallet's USDC balance in dollars and when it was read.
// The CLOB does not deduct resting orders from it: a working buy still counts
// as cash until it fills.
func (t *Tracker) Cash(ctx context.Context) (float64, time.Time, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if !t.cashAt.IsZero() && now.Sub(t.cashAt) < t.cashTTL {
		return t.cash, t.cashAt, nil
	}
	balance, err := t.balances.BalanceAllowance(ctx, "COLLATERAL", "")
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("read collateral balance: %w", err)
	}
	units, ok := decimal.Rat(balance.Balance)
	if !ok || units.Sign() < 0 {
		return 0, time.Time{}, fmt.Errorf("invalid collateral balance %q", balance.Balance)
	}
	cash, _ := new(big.Rat).Quo(units, big.NewRat(collateralUnit, 1)).Float64()
	t.cash, t.cashAt = cash, now
	return cash, now, nil
}

// Snapshot values the wallet now. It fails rather than guess when the cash
// balance cannot be read, since every size derived from it would be wrong.
func (t *Tracker) Snapshot(ctx context.Context) (Snapshot, error) {
	cash, cashAt, err := t.Cash(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	records, err := t.positions.PositionFeatures(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load positions for equity: %w", err)
	}
	now := t.now()
	snapshot := Snapshot{CashUSD: cash, CashAsOf: cashAt.UTC(), AsOf: now.UTC()}
	for _, record := range records {
		if !decimal.Positive(record.PositionSize) {
			continue
		}
		shares, err := decimal.PositiveFloat(record.PositionSize)
		if err != nil {
			return Snapshot{}, fmt.Errorf("position %s/%s/%s: %w", record.ConditionID, record.TokenID, record.UniqueTag, err)
		}
		price, marked := t.bid(record.ConditionID, record.TokenID, now)
		if !marked {
			snapshot.UnmarkedPositions++
			price, err = decimal.NonNegativeFloat(record.EntryPrice)
			if err != nil {
				return Snapshot{}, fmt.Errorf("position %s/%s/%s has no quote and no usable entry price: %w", record.ConditionID, record.TokenID, record.UniqueTag, err)
			}
		}
		snapshot.Positions++
		snapshot.PositionsUSD += shares * price
	}
	snapshot.EquityUSD = snapshot.CashUSD + snapshot.PositionsUSD
	return snapshot, nil
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
