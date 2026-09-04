package accountfeed

import (
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
)

// AccountFill is an in-process normalized event from the authenticated user stream.
type AccountFill struct {
	SchemaVersion   string        `json:"schema_version"`
	FillID          string        `json:"fill_id"`
	ExchangeOrderID string        `json:"exchange_order_id,omitempty"`
	IntentID        string        `json:"intent_id,omitempty"`
	MarketID        string        `json:"market_id,omitempty"`
	ConditionID     string        `json:"condition_id"`
	TokenID         string        `json:"token_id"`
	Outcome         string        `json:"outcome"`
	Side            protocol.Side `json:"side"`
	Shares          string        `json:"shares"`
	Price           string        `json:"price"`
	Fee             string        `json:"fee,omitempty"`
	FeeRateBps      string        `json:"fee_rate_bps,omitempty"`
	TradeStatus     string        `json:"trade_status,omitempty"`
	TraderSide      string        `json:"trader_side,omitempty"`
	ExchangeTime    time.Time     `json:"exchange_time"`
	ReceivedAt      time.Time     `json:"received_at"`
}

type AccountTrade struct {
	SchemaVersion string             `json:"schema_version,omitempty"`
	ID            string             `json:"id"`
	TakerOrderID  string             `json:"taker_order_id"`
	Market        string             `json:"market"`
	AssetID       string             `json:"asset_id"`
	Side          protocol.Side      `json:"side"`
	Size          string             `json:"size"`
	Price         string             `json:"price"`
	Outcome       string             `json:"outcome"`
	Status        string             `json:"status"`
	FeeRateBps    string             `json:"fee_rate_bps"`
	TraderSide    string             `json:"trader_side"`
	Owner         string             `json:"owner"`
	TradeOwner    string             `json:"trade_owner"`
	Timestamp     string             `json:"timestamp"`
	MakerOrders   []AccountMakerFill `json:"maker_orders"`
}

type AccountMakerFill struct {
	OrderID       string        `json:"order_id"`
	Owner         string        `json:"owner"`
	MatchedAmount string        `json:"matched_amount"`
	Price         string        `json:"price"`
	AssetID       string        `json:"asset_id"`
	Outcome       string        `json:"outcome"`
	Side          protocol.Side `json:"side"`
}

// AccountOrderEvent is an in-process normalized event from the authenticated user stream.
type AccountOrderEvent struct {
	SchemaVersion   string    `json:"schema_version"`
	EventID         string    `json:"event_id"`
	ExchangeOrderID string    `json:"exchange_order_id"`
	IntentID        string    `json:"intent_id,omitempty"`
	ConditionID     string    `json:"condition_id,omitempty"`
	TokenID         string    `json:"token_id,omitempty"`
	Status          string    `json:"status"`
	MatchedShares   string    `json:"matched_shares,omitempty"`
	AveragePrice    string    `json:"average_price,omitempty"`
	ExchangeTime    time.Time `json:"exchange_time"`
	ReceivedAt      time.Time `json:"received_at"`
}
