package tactics

import (
	"testing"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
)

// A preserve sells the complement sized by the inventory that backs it, at
// its limit rounded up onto the grid, as any sell is.
func TestPlanSplitPreserveSellsThePreservedShares(t *testing.T) {
	request := sellRequest(protocol.ExecutionStyleSplitPreserve, "")
	request.Intent.Kind = protocol.IntentOpen
	request.Intent.LimitPrice = "0.405"
	request.Intent.TargetUSD = "50"
	request.Intent.Policy.Preserve = &protocol.PreserveLeg{RequestedShares: "500", Shares: "100"}
	decision := mustPlan(t, request)
	if decimal.Compare(decision.Shares, "100") != 0 || decision.Price != "0.41" {
		t.Fatalf("unexpected preserve plan: %+v", decision)
	}
}

// A preserve buying a favourite sells its cheap complement: crossing the bid
// under $1 at the limit price, the exchange would reject it, so it is refused
// here. Resting, or worth $1 or more, it goes through.
func TestPlanSplitPreserveRefusesAMarketableSellUnderOneDollar(t *testing.T) {
	cases := []struct {
		name, limit, shares string
		refused             bool
	}{
		{"crossing under $1", "0.10", "5", true},   // bid 0.52: $0.50
		{"crossing at $1", "0.10", "10", false},    // $1.00
		{"resting under $1", "0.60", "1.5", false}, // above the bid: rests
	}
	for _, tc := range cases {
		request := sellRequest(protocol.ExecutionStyleSplitPreserve, "")
		request.Intent.TokenID = "down-token" // bid 0.52
		request.Intent.Kind = protocol.IntentOpen
		request.Intent.LimitPrice = tc.limit
		request.Intent.Policy.Preserve = &protocol.PreserveLeg{RequestedShares: tc.shares, Shares: tc.shares}
		_, err := Plan(request)
		if refused := BelowMinOrderSize(err); refused != tc.refused {
			t.Fatalf("%s: err %v, refused=%v, want %v", tc.name, err, refused, tc.refused)
		}
	}
}
