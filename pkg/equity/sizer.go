package equity

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
)

// DefaultMaxFraction bounds a single entry's share of equity. It is a guard
// against a unit mistake upstream (3 for 3%), not a risk limit.
const DefaultMaxFraction = 0.25

var (
	// ErrInvalidFraction means the requested fraction is not in (0, max].
	ErrInvalidFraction = errors.New("invalid equity fraction")
	// ErrInsufficientCash means the entry costs more than the cash not already
	// committed to working buys.
	ErrInsufficientCash = errors.New("insufficient free cash")
)

// OpenBuySource reports the USD committed to working buy orders, which the
// CLOB still counts as cash.
type OpenBuySource interface {
	OpenBuyNotional(context.Context) (string, error)
}

// Entry is a sized entry. Release must be called once the entry's buy has been
// reserved in the store, or has failed, so its hold on free cash ends.
type Entry struct {
	TargetUSD string
	EquityUSD float64
	release   func()
}

func (e Entry) Release() {
	if e.release != nil {
		e.release()
	}
}

// Sizer turns a fraction of equity into a dollar amount for one entry.
//
// Equity does not move when a buy is placed (the order is still cash until it
// fills), so concurrent entries all size from the same base, as Kelly intends.
// What they can exhaust is free cash, which is why each entry holds its
// amount until the store's reservation takes over, and why the check of free
// cash and the hold are made together under one lock.
type Sizer struct {
	tracker     *Tracker
	openBuys    OpenBuySource
	maxFraction float64
	// anchor, when set, replaces live equity as the base (SetLossAnchor).
	anchor *LossAnchor

	mu      sync.Mutex
	pending float64
	// releases counts entries whose hold has ended. A hold ends once the
	// entry's reservation is in the store, so a count that moved while
	// open buys were being read means the read may have missed one.
	releases uint64
}

// maxSizeAttempts bounds how often Size re-reads after an entry was released
// mid-read before it reads with the lock held, as every entry once did.
const maxSizeAttempts = 3

func NewSizer(tracker *Tracker, openBuys OpenBuySource) (*Sizer, error) {
	if tracker == nil || openBuys == nil {
		return nil, fmt.Errorf("equity tracker and open buy source are required")
	}
	return &Sizer{tracker: tracker, openBuys: openBuys, maxFraction: DefaultMaxFraction}, nil
}

func (s *Sizer) SetMaxFraction(max float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxFraction = max
}

// SetLossAnchor sizes entries from the equity anchored at the latest loss
// instead of the wallet's live equity. Free cash is still read live.
func (s *Sizer) SetLossAnchor(anchor *LossAnchor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.anchor = anchor
}

// Size resolves fraction × equity into a target in USD, checked against free
// cash: the balance less working buys and entries still being placed.
//
// The balance and open buys are read without the lock, so concurrent entries
// do not wait out each other's round trips. That is safe unless an entry is
// released in between: its hold is gone from pending, and if the read of open
// buys came before its reservation, it is counted nowhere. Size then reads
// again, and after maxSizeAttempts holds the lock across the read.
func (s *Sizer) Size(ctx context.Context, fraction string) (Entry, error) {
	f, err := strconv.ParseFloat(fraction, 64)
	s.mu.Lock()
	maxFraction, anchor := s.maxFraction, s.anchor
	s.mu.Unlock()
	if err != nil || math.IsNaN(f) || f <= 0 || f > maxFraction {
		return Entry{}, fmt.Errorf("%w %q: must be in (0, %g]", ErrInvalidFraction, fraction, maxFraction)
	}
	for attempt := 1; ; attempt++ {
		locked := attempt >= maxSizeAttempts
		s.mu.Lock()
		releases := s.releases
		if !locked {
			s.mu.Unlock()
		}
		equityUSD, cash, openBuys, err := s.read(ctx, anchor)
		if !locked {
			s.mu.Lock()
		}
		if err == nil && !locked && s.releases != releases {
			s.mu.Unlock()
			continue
		}
		entry, err := s.hold(f, equityUSD, cash, openBuys, err)
		s.mu.Unlock()
		return entry, err
	}
}

// read loads the base equity, the wallet's cash and the USD in working buys.
func (s *Sizer) read(ctx context.Context, anchor *LossAnchor) (equityUSD, cash, openBuys float64, err error) {
	equityUSD, cash, err = s.base(ctx, anchor)
	if err != nil {
		return 0, 0, 0, err
	}
	committed, err := s.openBuys.OpenBuyNotional(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("load open buy notional: %w", err)
	}
	openBuys, err = decimal.NonNegativeFloat(committed)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("open buy notional: %w", err)
	}
	return equityUSD, cash, openBuys, nil
}

// hold checks the entry against free cash and claims it. s.mu must be held.
func (s *Sizer) hold(f, equityUSD, cash, openBuys float64, readErr error) (Entry, error) {
	if readErr != nil {
		return Entry{}, readErr
	}
	free := cash - openBuys - s.pending
	// Floor to USDC precision so the entry never exceeds its share.
	target := math.Floor(f*equityUSD*1e6) / 1e6
	if target <= 0 {
		return Entry{}, fmt.Errorf("%w: equity %.6f gives no entry at fraction %g", ErrInsufficientCash, equityUSD, f)
	}
	if target > free {
		return Entry{}, fmt.Errorf("%w: entry needs %.6f, free cash is %.6f", ErrInsufficientCash, target, math.Max(free, 0))
	}
	s.pending += target
	var once sync.Once
	return Entry{
		TargetUSD: strconv.FormatFloat(target, 'f', -1, 64),
		EquityUSD: equityUSD,
		release: func() {
			once.Do(func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.pending -= target
				s.releases++
			})
		},
	}, nil
}

// base is the equity an entry is a fraction of, and the wallet's cash.
func (s *Sizer) base(ctx context.Context, anchor *LossAnchor) (equityUSD, cash float64, err error) {
	if anchor == nil {
		snapshot, err := s.tracker.Snapshot(ctx)
		return snapshot.EquityUSD, snapshot.CashUSD, err
	}
	// Until the first base is recorded, which waits for any unredeemed
	// winner to settle, entries size from live equity rather than fail.
	if equityUSD, err = anchor.Equity(); errors.Is(err, ErrNoSizingBase) {
		snapshot, err := s.tracker.Snapshot(ctx)
		return snapshot.EquityUSD, snapshot.CashUSD, err
	} else if err != nil {
		return 0, 0, err
	}
	cash, _, err = s.tracker.Cash(ctx)
	return equityUSD, cash, err
}
