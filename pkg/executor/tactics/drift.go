package tactics

import (
	"fmt"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
)

// DefaultDriftQuoteMaxAge is how old a quote may be and still cancel an order
// for drift when the policy names no quote_max_age_ms. A stale quote says
// nothing about where the market is now, and an order is never cancelled on
// one.
const DefaultDriftQuoteMaxAge = 2 * time.Second

// Drift is an open's parsed drift bound. The zero value is no bound.
type Drift struct {
	Signal float64
	Max    float64
}

// Bounded reports whether the open carries a bound at all.
func (d Drift) Bounded() bool { return d.Max > 0 && d.Signal > 0 }

// ParseDrift reads an intent's signal_price and max_signal_drift. Both must be
// set or both empty, and only an order that rests as GTC may carry them: an
// immediate order is matched before any later quote could withdraw it.
func ParseDrift(intent protocol.ExecutionIntent) (Drift, error) {
	signal, bound := strings.TrimSpace(intent.SignalPrice), strings.TrimSpace(intent.MaxSignalDrift)
	if signal == "" && bound == "" {
		return Drift{}, nil
	}
	if signal == "" || bound == "" {
		return Drift{}, fmt.Errorf("signal_price and max_signal_drift must be set together")
	}
	price, err := decimal.Price(signal)
	if err != nil {
		return Drift{}, fmt.Errorf("invalid signal_price %q", intent.SignalPrice)
	}
	maxDrift, err := decimal.PositiveFloat(bound)
	if err != nil || maxDrift >= 1 {
		return Drift{}, fmt.Errorf("invalid max_signal_drift %q", intent.MaxSignalDrift)
	}
	// Judge the order as it will be sent: a TAKER_AGGRESSIVE GTC goes out
	// as a FAK.
	if tif := timeInForce(intent, intent.Policy.Style); tif != protocol.TimeInForceGTC {
		if tif != intent.TimeInForce {
			return Drift{}, fmt.Errorf("max_signal_drift needs a resting GTC order, but style %s sends it as %s", intent.Policy.Style, tif)
		}
		return Drift{}, fmt.Errorf("max_signal_drift needs a GTC order, not %s", tif)
	}
	return Drift{Signal: price, Max: maxDrift}, nil
}

// DriftBreach reports whether the market has moved against an open by more
// than its bound, and the mid it judged that on. Only an adverse move counts:
// a BUY whose mid fell more than the bound under the signal, a SELL whose mid
// rose more than it above. A move in the entry's favour is what the entry
// waits for, so it never cancels one. It judges nothing, and reports false,
// without a bound or without a fresh two-sided quote for the intent's token —
// the same reading pmm's backtest venue makes (retireIfDrifted).
func DriftBreach(intent protocol.ExecutionIntent, drift Drift, quote marketquotes.Snapshot, hasQuote bool, now time.Time) (float64, bool) {
	if !drift.Bounded() {
		return 0, false
	}
	side, ok := driftSide(intent, quote, hasQuote, now)
	if !ok {
		return 0, false
	}
	mid := (side.Bid + side.Ask) / 2
	const epsilon = 1e-9
	if intent.Side == protocol.SideSell {
		return mid, mid-drift.Signal > drift.Max+epsilon
	}
	return mid, drift.Signal-mid > drift.Max+epsilon
}

// FreshForDrift reports whether quote carries a two-sided price for the
// intent's token young enough for the drift check: the intent's
// quote_max_age_ms, or DefaultDriftQuoteMaxAge.
func FreshForDrift(intent protocol.ExecutionIntent, quote marketquotes.Snapshot, now time.Time) bool {
	_, ok := driftSide(intent, quote, true, now)
	return ok
}

func driftSide(intent protocol.ExecutionIntent, quote marketquotes.Snapshot, hasQuote bool, now time.Time) (marketquotes.Quote, bool) {
	if intent.Policy.QuoteMaxAgeMillis <= 0 {
		intent.Policy.QuoteMaxAgeMillis = DefaultDriftQuoteMaxAge.Milliseconds()
	}
	side, ok := freshQuoteForToken(Request{Intent: intent, Quote: quote, HasQuote: hasQuote, Now: now})
	if !ok || side.Bid <= 0 || side.Ask <= 0 {
		return marketquotes.Quote{}, false
	}
	return side, true
}
