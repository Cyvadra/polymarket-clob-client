package accountfeed

import (
	"math/big"
	"strings"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
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
	exchangeTime := trade.Time()
	fills := make([]AccountFill, 0, len(trade.MakerOrders)+1)
	if trade.TakerOrderID != "" && ownsTakerTrade(trade.TraderSide) {
		fills = append(fills, AccountFill{SchemaVersion: protocol.SchemaVersionV1, FillID: trade.ID, ExchangeOrderID: trade.TakerOrderID, MarketID: trade.Market, ConditionID: trade.Market, TokenID: trade.AssetID, Outcome: trade.Outcome, Side: trade.Side, Shares: trade.Size, Price: takerPrice(trade), FeeRateBps: trade.FeeRateBps, TradeStatus: status, TraderSide: trade.TraderSide, ExchangeTime: exchangeTime, ReceivedAt: receivedAt})
	}
	for _, maker := range trade.MakerOrders {
		if maker.OrderID == "" || maker.Outcome == "" || !ownsOrder(maker.Owner, apiKey) {
			continue
		}
		fills = append(fills, AccountFill{SchemaVersion: protocol.SchemaVersionV1, FillID: trade.ID + ":" + maker.OrderID, ExchangeOrderID: maker.OrderID, MarketID: trade.Market, ConditionID: trade.Market, TokenID: maker.AssetID, Outcome: maker.Outcome, Side: maker.Side, Shares: maker.MatchedAmount, Price: maker.Price, FeeRateBps: trade.FeeRateBps, TradeStatus: status, TraderSide: "MAKER", ExchangeTime: exchangeTime, ReceivedAt: receivedAt})
	}
	return fills
}

// takerPrice is what a taker paid or got per share: the average over the
// maker orders it matched, weighted by the shares each filled. The trade's own
// price is a single level, so a taker order that swept the book would be
// booked at the wrong cost. A maker on the other outcome token was matched at
// the complement of its price. Without usable maker orders it falls back to
// the trade's price.
func takerPrice(trade clobclient.Trade) string {
	if len(trade.MakerOrders) == 0 {
		return trade.Price
	}
	one := big.NewRat(1, 1)
	shares, cost := new(big.Rat), new(big.Rat)
	for _, maker := range trade.MakerOrders {
		matched, ok := decimal.Rat(maker.MatchedAmount)
		if !ok || matched.Sign() <= 0 {
			return trade.Price
		}
		price, ok := decimal.Rat(maker.Price)
		if !ok || price.Sign() <= 0 || price.Cmp(one) >= 0 {
			return trade.Price
		}
		if maker.AssetID != trade.AssetID {
			price.Sub(one, price)
		}
		shares.Add(shares, matched)
		cost.Add(cost, price.Mul(price, matched))
	}
	average := strings.TrimRight(cost.Quo(cost, shares).FloatString(12), "0")
	return strings.TrimSuffix(average, ".")
}

func ownsTakerTrade(traderSide string) bool {
	return strings.EqualFold(strings.TrimSpace(traderSide), "TAKER")
}

func ownsOrder(owner, apiKey string) bool {
	return strings.EqualFold(strings.TrimSpace(owner), strings.TrimSpace(apiKey))
}
