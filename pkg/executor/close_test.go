package executor

import (
	"context"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

// A lane's position row survives its close, so an emptied row must not claim
// the lane: a close for it belongs to whichever wallet trades it now.
func TestHoldsLaneRequiresShares(t *testing.T) {
	storer := &fakeStore{positions: []store.PositionRecord{
		{ConditionID: "c", TokenID: "held", UniqueTag: "lane", ActualShares: "5"},
		{ConditionID: "c", TokenID: "emptied", UniqueTag: "lane", ActualShares: "0", State: "empty"},
	}}
	exec, err := New(storer, &fakeCLOB{}, time.Now)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	for token, want := range map[string]bool{"held": true, "emptied": false, "missing": false} {
		got, err := exec.HoldsLane(context.Background(), protocol.ExecutionCloseRequest{ConditionID: "c", AssetID: token, UniqueTag: "lane"})
		if err != nil || got != want {
			t.Errorf("%s: HoldsLane = %v, %v; want %v", token, got, err, want)
		}
	}
}
