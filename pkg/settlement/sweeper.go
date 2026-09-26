package settlement

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// Store is what the sweeper reads and writes.
type Store interface {
	store.PositionStore
	store.PositionSettler
}

// Sweeper empties lanes whose markets have resolved and whose shares are
// worth nothing: a losing token, or a winner the wallet no longer holds
// because it was redeemed. A winner still in the wallet is only shrunk to the
// wallet's balance and stays until it is redeemed, since it is still worth $1
// a share. Lanes holding the same token share one wallet balance, which is
// handed out to them in table order, so two lanes cannot both claim it.
// Everything goes through SettlePosition, which only ever lowers a lane, so a
// sweep cannot raise one; the revision it bumps republishes the lane, and the
// strategy sees it flat.
//
// A lane with shares reserved is left alone: a close is working on it, and
// the close path owns it until the reservation ends. A lane that changed
// within SettleGrace is left alone too, since its tokens may not have landed
// in the wallet yet and a low balance says nothing about it.
type Sweeper struct {
	store    Store
	resolver *Resolver
	now      func() time.Time
	interval time.Duration
	onError  func(error)
	onSwept  func(Sweep)
}

// Sweep reports one pass. Settling counts the winners left alone because
// their lane changed too recently for the wallet balance to be trusted;
// Reserved those left to a working close.
type Sweep struct {
	Checked, Lost, Redeemed, Shrunk, Reserved, Settling int
}

func NewSweeper(s Store, resolver *Resolver, now func() time.Time, interval time.Duration) (*Sweeper, error) {
	if s == nil || resolver == nil {
		return nil, fmt.Errorf("store and resolver are required")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("sweep interval must be positive")
	}
	if now == nil {
		now = time.Now
	}
	return &Sweeper{store: s, resolver: resolver, now: now, interval: interval}, nil
}

func (s *Sweeper) SetErrorHandler(handler func(error)) { s.onError = handler }

// SetSweepHandler observes every pass that changed a lane or skipped one.
func (s *Sweeper) SetSweepHandler(handler func(Sweep)) { s.onSwept = handler }

func (s *Sweeper) Init(context.Context) error  { return nil }
func (s *Sweeper) Close(context.Context) error { return nil }

func (s *Sweeper) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		s.pass(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Sweeper) pass(ctx context.Context) {
	sweep, err := s.Sweep(ctx)
	if err != nil && s.onError != nil {
		s.onError(err)
	}
	if s.onSwept != nil && sweep.Lost+sweep.Redeemed+sweep.Shrunk+sweep.Reserved+sweep.Settling > 0 {
		s.onSwept(sweep)
	}
}

// Sweep runs one pass over every lane holding shares. One lane's error does
// not stop the rest; it is retried on the next pass.
func (s *Sweeper) Sweep(ctx context.Context) (Sweep, error) {
	var sweep Sweep
	positions, err := s.store.PositionFeatures(ctx)
	if err != nil {
		return sweep, fmt.Errorf("load positions to sweep: %w", err)
	}
	now := s.now()
	var errs error
	// What is left of each winning token's wallet balance after the lanes
	// seen so far, in the store's order, took their share.
	remaining := map[string]float64{}
	for _, p := range positions {
		if !decimal.Positive(p.PositionSize) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return sweep, errors.Join(errs, err)
		}
		sweep.Checked++
		lane := p.ConditionID + "/" + p.TokenID + "/" + p.UniqueTag
		outcome, err := s.resolver.Outcome(ctx, p.ConditionID, p.TokenID)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("sweep %s: %w", lane, err))
			continue
		}
		if !outcome.Resolved {
			continue
		}
		size, err := decimal.PositiveFloat(p.PositionSize)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("sweep %s: %w", lane, err))
			continue
		}
		if outcome.Winner {
			if _, seen := remaining[p.TokenID]; !seen {
				remaining[p.TokenID] = outcome.WalletShares
			}
		}
		// A lane that is left alone keeps its claim on the wallet balance.
		switch {
		case decimal.Positive(p.ReservedSize):
			sweep.Reserved++
			remaining[p.TokenID] -= size
			continue
		case outcome.Winner && !Settled(p, now):
			sweep.Settling++
			remaining[p.TokenID] -= size
			continue
		}
		var keep float64
		switch {
		case !outcome.Winner:
		case remaining[p.TokenID] <= 0:
		case remaining[p.TokenID] < size:
			keep = remaining[p.TokenID]
		default:
			remaining[p.TokenID] -= size
			continue // a winner the wallet still holds in full
		}
		remaining[p.TokenID] -= keep
		shares := strconv.FormatFloat(keep, 'f', 6, 64)
		changed, err := s.store.SettlePosition(ctx, p.ConditionID, p.TokenID, p.UniqueTag, shares)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("sweep %s: %w", lane, err))
			continue
		}
		if !changed {
			// A close reserved the lane between the read and the write, or
			// it already shrank; either way the store declined and the lane
			// is not counted as swept.
			sweep.Reserved++
			continue
		}
		switch {
		case !outcome.Winner:
			sweep.Lost++
		case keep == 0:
			sweep.Redeemed++
		default:
			sweep.Shrunk++
		}
	}
	return sweep, errs
}
