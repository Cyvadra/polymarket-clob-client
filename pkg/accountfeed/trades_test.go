package accountfeed

import (
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
)

func TestOwnedFillsFromTradeFiltersToOwnedOrders(t *testing.T) {
	receivedAt := time.Unix(20, 0).UTC()
	fills := OwnedFillsFromTrade(clobclient.Trade{
		ID: "trade-1", TakerOrderID: "other-taker", Market: "condition", AssetID: "token", Side: protocol.SideBuy,
		Size: "2", Price: "0.5", Outcome: "Up", Status: "TRADE_STATUS_MATCHED", TraderSide: "MAKER", Timestamp: "1000",
		MakerOrders: []clobclient.MakerTrade{
			{OrderID: "owned-maker", Owner: "key", MatchedAmount: "1", Price: "0.5", AssetID: "token", Outcome: "Up", Side: protocol.SideSell},
			{OrderID: "other-maker", Owner: "someone", MatchedAmount: "1", Price: "0.5", AssetID: "token", Outcome: "Up", Side: protocol.SideSell},
		},
	}, "key", receivedAt)
	if len(fills) != 1 || fills[0].ExchangeOrderID != "owned-maker" || fills[0].TraderSide != "MAKER" || !fills[0].ReceivedAt.Equal(receivedAt) {
		t.Fatalf("expected only owned maker fill, got %+v", fills)
	}
}

// A taker order that sweeps several levels pays each maker's price, not the
// trade's single price; a maker on the other token matched at its complement.
func TestOwnedFillsFromTradeBooksATakerAtItsAveragePrice(t *testing.T) {
	fills := OwnedFillsFromTrade(clobclient.Trade{ID: "trade-1", TakerOrderID: "order-1", Market: "condition", AssetID: "up", Side: protocol.SideBuy, Size: "10", Price: "0.93", Outcome: "Up", Status: "CONFIRMED", TraderSide: "TAKER", Timestamp: "1000",
		MakerOrders: []clobclient.MakerTrade{
			{OrderID: "a", Owner: "someone", MatchedAmount: "4", Price: "0.93", AssetID: "up", Outcome: "Up", Side: protocol.SideSell},
			{OrderID: "b", Owner: "someone", MatchedAmount: "6", Price: "0.05", AssetID: "down", Outcome: "Down", Side: protocol.SideBuy},
		}}, "key", time.Unix(20, 0).UTC())
	if len(fills) != 1 || fills[0].Price != "0.942" || fills[0].Shares != "10" {
		t.Fatalf("expected one taker fill of 10 at 0.942, got %+v", fills)
	}
}

func TestOwnedFillsFromTradeKeepsOwnedTakerTrade(t *testing.T) {
	fills := OwnedFillsFromTrade(clobclient.Trade{ID: "trade-1", TakerOrderID: "order-1", Market: "condition", AssetID: "token", Side: protocol.SideBuy, Size: "2", Price: "0.5", Outcome: "Up", Status: "CONFIRMED", TraderSide: "TAKER", Timestamp: "1000"}, "key", time.Unix(20, 0).UTC())
	if len(fills) != 1 || fills[0].ExchangeOrderID != "order-1" || fills[0].TraderSide != "TAKER" {
		t.Fatalf("expected owned taker fill, got %+v", fills)
	}
}
