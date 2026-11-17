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
