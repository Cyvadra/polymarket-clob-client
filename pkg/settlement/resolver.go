// Package settlement handles the lanes whose markets have ended. executiond
// never learns that a market settled from its own feeds: its lanes keep their
// shares after the end, and the market drops out of the PMM quotes. The CLOB
// knows how each market resolved, and the wallet's token balance says whether
// a winner is still there to redeem. The Resolver reads both; the Sweeper uses
// it to empty lanes that hold nothing of value, and pkg/equity to value the
// rest.
package settlement

import (
	"context"
	"fmt"
	"sync"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// MarketReader reads a market's resolution from the CLOB.
type MarketReader interface {
	Market(ctx context.Context, conditionID string) (*clobclient.Market, error)
}

// BalanceReader reads the wallet's balance of an outcome token, in base units.
// It must come from the chain, not the CLOB: the CLOB's balance endpoints
// refuse a token once its market's order book is removed ("No orderbook
// exists"), which is exactly when a settled lane needs valuing, and its
// cached view can lag the chain, where a stale zero reads as a redeemed winner.
type BalanceReader interface {
	TokenBalance(ctx context.Context, tokenID string) (string, error)
}

const (
	// openMarketTTL is how long a market that has not resolved yet is taken at
	// its word before asking again. Up/down markets resolve minutes after they
	// close.
	openMarketTTL = 30 * time.Second
	// winnerBalanceTTL is how long a winning token's zero wallet balance is
	// reused. A stale zero can only undercount, if tokens from a fill that
	// settled late arrive, and those lanes are valued at their recorded
	// shares within SettleGrace.
	winnerBalanceTTL = time.Minute
	// heldBalanceTTL is how long a balance above zero is reused: only long
	// enough for one pass over the lanes sharing a token. Redemption,
	// automatic minutes after the market ends, moves the balance into cash,
	// and a stale read counts it twice for as long as it is reused.
	heldBalanceTTL = 2 * time.Second
	// failureTTL is how long a failed lookup is held before it is tried again,
	// so a lane the CLOB cannot serve costs one request per interval rather
	// than one per valuation.
	failureTTL = openMarketTTL
	// idleTTL is how long an entry nobody has asked about stays cached. Every
	// market ever traded passes through here, so entries whose lanes have been
	// swept are dropped instead of accumulating for the life of the process.
	idleTTL = 10 * time.Minute
	// SettleGrace is how long after a lane last changed the wallet's balance
	// of its token is trusted to be lower than the lane. A fill is booked
	// before its tokens land in the wallet, so a lane bought into just now can
	// read as a balance of zero: within the grace such a lane is taken to
	// hold what it recorded.
	SettleGrace = 2 * time.Minute
)

type resolution struct {
	resolved bool
	tokens   map[string]bool
	winners  map[string]bool
	err      error
	at, used time.Time
}

type tokenBalance struct {
	shares   float64
	err      error
	at, used time.Time
}

// Outcome is what one token of a market is worth after it ended.
type Outcome struct {
	// Resolved is false while the market is open or still being resolved;
	// nothing else is set then.
	Resolved bool
	Winner   bool
	// WalletShares is the wallet's balance of a winning token: what is left
	// to redeem at $1. It is not read for a loser, which is worth nothing.
	WalletShares float64
}

type Resolver struct {
	markets  MarketReader
	balances BalanceReader
	now      func() time.Time

	mu       sync.Mutex
	resolved map[string]resolution
	wallet   map[string]tokenBalance
	pruned   time.Time
}

func NewResolver(markets MarketReader, balances BalanceReader, now func() time.Time) (*Resolver, error) {
	if markets == nil || balances == nil {
		return nil, fmt.Errorf("market reader and balance reader are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Resolver{
		markets: markets, balances: balances, now: now,
		resolved: map[string]resolution{}, wallet: map[string]tokenBalance{},
	}, nil
}

// Settled reports whether a lane has been still for long enough that the
// wallet's balance of its token can be trusted to have caught up with it. A
// lane whose last trade has not confirmed on chain is never settled, however
// old: the store marks it unsettled until the trade confirms, and its tokens
// may not have reached the wallet yet.
func Settled(record store.PositionRecord, now time.Time) bool {
	if record.State == "unsettled" {
		return false
	}
	return now.Sub(record.UpdatedAt) >= SettleGrace
}

// Outcome reports how tokenID's market resolved, if it has. A token the
// market does not list is an error, not a loser: the lane's condition and
// token ids do not describe the same market, and nothing can be said about
// what it holds.
func (r *Resolver) Outcome(ctx context.Context, conditionID, tokenID string) (Outcome, error) {
	res, err := r.resolution(ctx, conditionID)
	if err != nil || !res.resolved {
		return Outcome{}, err
	}
	if !res.tokens[tokenID] {
		return Outcome{}, fmt.Errorf("token %s is not in market %s", tokenID, conditionID)
	}
	if !res.winners[tokenID] {
		return Outcome{Resolved: true}, nil
	}
	held, err := r.walletShares(ctx, tokenID)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Resolved: true, Winner: true, WalletShares: held}, nil
}

func (r *Resolver) resolution(ctx context.Context, conditionID string) (resolution, error) {
	now := r.now()
	r.mu.Lock()
	r.prune(now)
	cached, ok := r.resolved[conditionID]
	if ok {
		cached.used = now
		r.resolved[conditionID] = cached
	}
	r.mu.Unlock()
	if ok {
		switch {
		case cached.err != nil && now.Sub(cached.at) < failureTTL:
			return resolution{}, cached.err
		// A resolution is final and never asked again.
		case cached.err == nil && (cached.resolved || now.Sub(cached.at) < openMarketTTL):
			return cached, nil
		}
	}
	res := resolution{at: now, used: now, tokens: map[string]bool{}, winners: map[string]bool{}}
	market, err := r.markets.Market(ctx, conditionID)
	if err != nil {
		res.err = fmt.Errorf("read market %s: %w", conditionID, err)
	} else {
		for _, token := range market.Tokens {
			res.tokens[token.TokenID] = true
			if token.Winner {
				res.winners[token.TokenID] = true
			}
		}
		// A closed market with no winner yet is still being resolved.
		res.resolved = market.Closed && len(res.winners) > 0
	}
	r.mu.Lock()
	r.resolved[conditionID] = res
	r.mu.Unlock()
	return res, res.err
}

func (r *Resolver) walletShares(ctx context.Context, tokenID string) (float64, error) {
	now := r.now()
	r.mu.Lock()
	cached, ok := r.wallet[tokenID]
	if ok {
		cached.used = now
		r.wallet[tokenID] = cached
	}
	r.mu.Unlock()
	if ok {
		switch {
		case cached.err != nil && now.Sub(cached.at) < failureTTL:
			return 0, cached.err
		case cached.err == nil && cached.shares == 0 && now.Sub(cached.at) < winnerBalanceTTL,
			cached.err == nil && now.Sub(cached.at) < heldBalanceTTL:
			return cached.shares, nil
		}
	}
	entry := tokenBalance{at: now, used: now}
	balance, err := r.balances.TokenBalance(ctx, tokenID)
	if err != nil {
		entry.err = fmt.Errorf("read token balance %s: %w", tokenID, err)
	} else if entry.shares, err = decimal.FromBaseUnits(balance); err != nil {
		entry.err = fmt.Errorf("token balance %s: %w", tokenID, err)
	}
	r.mu.Lock()
	r.wallet[tokenID] = entry
	r.mu.Unlock()
	return entry.shares, entry.err
}

// prune drops entries nobody has asked about for idleTTL. Called with mu held.
func (r *Resolver) prune(now time.Time) {
	if now.Sub(r.pruned) < idleTTL {
		return
	}
	r.pruned = now
	for id, res := range r.resolved {
		if now.Sub(res.used) >= idleTTL {
			delete(r.resolved, id)
		}
	}
	for id, bal := range r.wallet {
		if now.Sub(bal.used) >= idleTTL {
			delete(r.wallet, id)
		}
	}
}
