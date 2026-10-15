package tactics

import (
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
)

func driftIntent(side protocol.Side) protocol.ExecutionIntent {
	return protocol.ExecutionIntent{Side: side, TokenID: "up", TimeInForce: protocol.TimeInForceGTC, SignalPrice: "0.5000", MaxSignalDrift: "0.0300"}
}

func quoteAt(now time.Time, bid, ask float64) marketquotes.Snapshot {
	return marketquotes.Snapshot{ConditionID: "c", At: now, Up: marketquotes.Quote{AssetID: "up", Bid: bid, Ask: ask, Timestamp: now}}
}

func TestParseDrift(t *testing.T) {
	for _, tc := range []struct {
		name             string
		signal, bound    string
		tif              protocol.TimeInForce
		style            protocol.ExecutionStyle
		wantErr, bounded bool
	}{
		{name: "none", tif: protocol.TimeInForceFAK},
		{name: "gtc", signal: "0.5", bound: "0.03", tif: protocol.TimeInForceGTC, bounded: true},
		{name: "half", signal: "0.5", tif: protocol.TimeInForceGTC, wantErr: true},
		{name: "fak", signal: "0.5", bound: "0.03", tif: protocol.TimeInForceFAK, wantErr: true},
		{name: "taker gtc goes out as fak", signal: "0.5", bound: "0.03", tif: protocol.TimeInForceGTC, style: protocol.ExecutionStyleTakerAggressive, wantErr: true},
		{name: "maker gtc", signal: "0.5", bound: "0.03", tif: protocol.TimeInForceGTC, style: protocol.ExecutionStyleMakerPostOnly, bounded: true},
		{name: "zero bound", signal: "0.5", bound: "0", tif: protocol.TimeInForceGTC, wantErr: true},
		{name: "bad signal", signal: "1.5", bound: "0.03", tif: protocol.TimeInForceGTC, wantErr: true},
	} {
		drift, err := ParseDrift(protocol.ExecutionIntent{SignalPrice: tc.signal, MaxSignalDrift: tc.bound, TimeInForce: tc.tif, Policy: protocol.ExecutionPolicy{Style: tc.style}})
		if (err != nil) != tc.wantErr || drift.Bounded() != tc.bounded {
			t.Errorf("%s: drift=%+v err=%v", tc.name, drift, err)
		}
	}
}

// Only a move against the entry cancels it: the mid falling under a BUY's
// signal, or rising over a SELL's. The entry is waiting for the other way.
func TestDriftBreachIsAdverseOnly(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	drift := Drift{Signal: 0.5, Max: 0.03}
	for _, tc := range []struct {
		name     string
		side     protocol.Side
		bid, ask float64
		want     bool
	}{
		{"buy inside", protocol.SideBuy, 0.47, 0.49, false},
		{"buy at the bound", protocol.SideBuy, 0.46, 0.48, false},
		{"buy fell away", protocol.SideBuy, 0.45, 0.47, true},
		{"buy ran up", protocol.SideBuy, 0.60, 0.62, false},
		{"sell rose away", protocol.SideSell, 0.53, 0.55, true},
		{"sell fell", protocol.SideSell, 0.40, 0.42, false},
	} {
		_, got := DriftBreach(driftIntent(tc.side), drift, quoteAt(now, tc.bid, tc.ask), true, now)
		if got != tc.want {
			t.Errorf("%s: breach=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// A quote that is stale, missing or for another token says nothing about where
// the market is, so it never cancels an order.
func TestDriftBreachNeedsAFreshQuoteForTheToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	drift := Drift{Signal: 0.5, Max: 0.03}
	intent := driftIntent(protocol.SideBuy)
	if _, got := DriftBreach(intent, drift, quoteAt(now.Add(-3*time.Second), 0.1, 0.12), true, now); got {
		t.Fatal("a quote older than the default max age must not cancel")
	}
	if _, got := DriftBreach(intent, drift, marketquotes.Snapshot{}, false, now); got {
		t.Fatal("no quote must not cancel")
	}
	other := quoteAt(now, 0.1, 0.12)
	other.Up.AssetID = "down"
	if _, got := DriftBreach(intent, drift, other, true, now); got {
		t.Fatal("another token's quote must not cancel")
	}
	if _, got := DriftBreach(intent, Drift{}, quoteAt(now, 0.1, 0.12), true, now); got {
		t.Fatal("an open without a bound must never cancel")
	}
}
