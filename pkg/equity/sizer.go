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
// What they can exhaust is free cash, which is why entries are serialized and
// each holds its amount until the store's reservation takes over.
type Sizer struct {
	tracker     *Tracker
	openBuys    OpenBuySource
	maxFraction float64
	// anchor, when set, replaces live equity as the base (SetLossAnchor).
	anchor *LossAnchor

	mu      sync.Mutex
	pending float64
}

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
func (s *Sizer) Size(ctx context.Context, fraction string) (Entry, error) {
	f, err := strconv.ParseFloat(fraction, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil || math.IsNaN(f) || f <= 0 || f > s.maxFraction {
		return Entry{}, fmt.Errorf("%w %q: must be in (0, %g]", ErrInvalidFraction, fraction, s.maxFraction)
	}
	equityUSD, cash, err := s.base(ctx)
	if err != nil {
		return Entry{}, err
	}
	committed, err := s.openBuys.OpenBuyNotional(ctx)
	if err != nil {
		return Entry{}, fmt.Errorf("load open buy notional: %w", err)
	}
	openBuys, err := decimal.NonNegativeFloat(committed)
	if err != nil {
		return Entry{}, fmt.Errorf("open buy notional: %w", err)
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
			})
		},
	}, nil
}

// base is the equity an entry is a fraction of, and the wallet's cash.
func (s *Sizer) base(ctx context.Context) (equityUSD, cash float64, err error) {
	if s.anchor == nil {
		snapshot, err := s.tracker.Snapshot(ctx)
		return snapshot.EquityUSD, snapshot.CashUSD, err
	}
	if equityUSD, err = s.anchor.Equity(); err != nil {
		return 0, 0, err
	}
	cash, _, err = s.tracker.Cash(ctx)
	return equityUSD, cash, err
}
