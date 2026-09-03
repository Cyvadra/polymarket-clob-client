package trading

import (
	"context"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/lifecycle"
)

func TestClosePositionWithGuardReservesAndConsumesFilledShares(t *testing.T) {
	inventory := lifecycle.NewInventory(nil)
	if err := inventory.RecordFill("token", 3); err != nil {
		t.Fatal(err)
	}
	broker := &fakeBroker{scripts: []*orderScript{{reads: []*clobclient.Order{{Status: "MATCHED", SizeMatched: "2", AvgPrice: ".4"}}}}}
	result, err := New(broker).ClosePositionWithGuard(context.Background(), CloseRequest{
		TokenID: "token", Shares: 2, LimitPrice: .4, CompleteWithin: time.Second,
	}, inventory)
	if err != nil || !result.Completed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	position, ok := inventory.Position("token")
	if !ok || position.Shares != 1 || position.Reserved != 0 {
		t.Fatalf("position=%+v", position)
	}
}
