package accountfeed

import (
	"strings"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
)

// OwnedFillsFromTrade normalizes a single account trade (from the user stream
// or the REST reconciliation replay) into the fills that belong to this
// executiond instance. Taker fills are attributed when the trade's trader_side
// is TAKER; maker fills are attributed per maker order whose owner matches the
// API key.
func OwnedFillsFromTrade(trade clobclient.Trade, apiKey string, receivedAt time.Time) []AccountFill {
	status := strings.TrimPrefix(strings.ToUpper(trade.Status), "TRADE_STATUS_")
	if (status != "MATCHED" && status != "MINED" && status != "CONFIRMED" && status != "FAILED") || trade.Outcome == "" {
		return nil
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	}
	exchangeTime := streamTime(trade.Timestamp)
	fills := make([]AccountFill, 0, len(trade.MakerOrders)+1)
	if trade.TakerOrderID != "" && ownsTakerTrade(trade.TraderSide) {
		fills = append(fills, AccountFill{SchemaVersion: protocol.SchemaVersionV1, FillID: trade.ID, ExchangeOrderID: trade.TakerOrderID, MarketID: trade.Market, ConditionID: trade.Market, TokenID: trade.AssetID, Outcome: trade.Outcome, Side: trade.Side, Shares: trade.Size, Price: trade.Price, FeeRateBps: trade.FeeRateBps, TradeStatus: status, TraderSide: trade.TraderSide, ExchangeTime: exchangeTime, ReceivedAt: receivedAt})
	}
	for _, maker := range trade.MakerOrders {
		if maker.OrderID == "" || maker.Outcome == "" || !ownsOrder(maker.Owner, apiKey) {
			continue
		}
		fills = append(fills, AccountFill{SchemaVersion: protocol.SchemaVersionV1, FillID: trade.ID + ":" + maker.OrderID, ExchangeOrderID: maker.OrderID, MarketID: trade.Market, ConditionID: trade.Market, TokenID: maker.AssetID, Outcome: maker.Outcome, Side: maker.Side, Shares: maker.MatchedAmount, Price: maker.Price, FeeRateBps: trade.FeeRateBps, TradeStatus: status, TraderSide: "MAKER", ExchangeTime: exchangeTime, ReceivedAt: receivedAt})
	}
	return fills
}

func ownsTakerTrade(traderSide string) bool {
	return strings.EqualFold(strings.TrimSpace(traderSide), "TAKER")
}

func ownsOrder(owner, apiKey string) bool {
	return strings.EqualFold(strings.TrimSpace(owner), strings.TrimSpace(apiKey))
}
