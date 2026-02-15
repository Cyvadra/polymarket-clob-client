package clobclient

import "time"

// Side represents the order side
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// OrderType represents the type of order
type OrderType string

const (
	OrderTypeGTC OrderType = "GTC" // Good Till Cancel
	OrderTypeFOK OrderType = "FOK" // Fill or Kill
	OrderTypeGTD OrderType = "GTD" // Good Till Date
	OrderTypeFAK OrderType = "FAK" // Fill and Kill
)

// Chain represents the blockchain network
type Chain int

const (
	ChainPolygon Chain = 137
	ChainAmoy    Chain = 80002
)

// SignatureType represents the signature type for orders
type SignatureType int

const (
	SignatureTypeEOA            SignatureType = 0
	SignatureTypePOLYPROXY      SignatureType = 1
	SignatureTypePOLYGNOSISSAFE SignatureType = 2
)

// AssetType represents the type of asset
type AssetType string

const (
	AssetTypeCollateral  AssetType = "COLLATERAL"
	AssetTypeConditional AssetType = "CONDITIONAL"
)

// TickSize represents the minimum price increment
type TickSize string

const (
	TickSize01   TickSize = "0.1"
	TickSize001  TickSize = "0.01"
	TickSize0001 TickSize = "0.001"
	TickSize00001 TickSize = "0.0001"
)

// PriceHistoryInterval represents the interval for price history
type PriceHistoryInterval string

const (
	PriceHistoryIntervalMax      PriceHistoryInterval = "max"
	PriceHistoryIntervalOneWeek  PriceHistoryInterval = "1w"
	PriceHistoryIntervalOneDay   PriceHistoryInterval = "1d"
	PriceHistoryIntervalSixHours PriceHistoryInterval = "6h"
	PriceHistoryIntervalOneHour  PriceHistoryInterval = "1h"
)

// ApiKeyCreds represents API key credentials
type ApiKeyCreds struct {
	Key        string `json:"key"`
	Secret     string `json:"secret"`
	Passphrase string `json:"passphrase"`
}

// ApiKeyRaw represents raw API key
type ApiKeyRaw struct {
	ApiKey     string `json:"apiKey"`
	Secret     string `json:"secret"`
	Passphrase string `json:"passphrase"`
}

// ReadonlyApiKeyResponse represents readonly API key response
type ReadonlyApiKeyResponse struct {
	ApiKey string `json:"apiKey"`
}

// L2HeaderArgs represents arguments for L2 headers
type L2HeaderArgs struct {
	Method      string
	RequestPath string
	Body        string
}

// SignedOrder represents a signed order
type SignedOrder struct {
	Salt          int64         `json:"salt"`
	Maker         string        `json:"maker"`
	Signer        string        `json:"signer"`
	Taker         string        `json:"taker"`
	TokenID       string        `json:"tokenId"`
	MakerAmount   string        `json:"makerAmount"`
	TakerAmount   string        `json:"takerAmount"`
	Expiration    string        `json:"expiration"`
	Nonce         string        `json:"nonce"`
	FeeRateBps    string        `json:"feeRateBps"`
	Side          Side          `json:"side"`
	SignatureType SignatureType `json:"signatureType"`
	Signature     string        `json:"signature"`
}

// UserOrder represents a simplified order for users
type UserOrder struct {
	TokenID    string  `json:"tokenID"`
	Price      float64 `json:"price"`
	Size       float64 `json:"size"`
	Side       Side    `json:"side"`
	FeeRateBps *int    `json:"feeRateBps,omitempty"`
	Nonce      *int64  `json:"nonce,omitempty"`
	Expiration *int64  `json:"expiration,omitempty"`
	Taker      *string `json:"taker,omitempty"`
}

// UserMarketOrder represents a simplified market order for users
type UserMarketOrder struct {
	TokenID    string     `json:"tokenID"`
	Price      *float64   `json:"price,omitempty"`
	Amount     float64    `json:"amount"`
	Side       Side       `json:"side"`
	FeeRateBps *int       `json:"feeRateBps,omitempty"`
	Nonce      *int64     `json:"nonce,omitempty"`
	Taker      *string    `json:"taker,omitempty"`
	OrderType  *OrderType `json:"orderType,omitempty"`
}

// PostOrderArgs represents arguments for posting an order
type PostOrderArgs struct {
	Order     SignedOrder `json:"order"`
	OrderType OrderType   `json:"orderType"`
	PostOnly  *bool       `json:"postOnly,omitempty"`
}

// OrderPayload represents the order ID payload
type OrderPayload struct {
	OrderID string `json:"orderID"`
}

// ApiKeysResponse represents API keys response
type ApiKeysResponse struct {
	ApiKeys []ApiKeyCreds `json:"apiKeys"`
}

// BanStatus represents ban status
type BanStatus struct {
	ClosedOnly bool `json:"closed_only"`
}

// OrderResponse represents the response from posting an order
type OrderResponse struct {
	Success            bool     `json:"success"`
	ErrorMsg           string   `json:"errorMsg"`
	OrderID            string   `json:"orderID"`
	TransactionsHashes []string `json:"transactionsHashes"`
	Status             string   `json:"status"`
	TakingAmount       string   `json:"takingAmount"`
	MakingAmount       string   `json:"makingAmount"`
}

// OpenOrder represents an open order
type OpenOrder struct {
	ID              string   `json:"id"`
	Status          string   `json:"status"`
	Owner           string   `json:"owner"`
	MakerAddress    string   `json:"maker_address"`
	Market          string   `json:"market"`
	AssetID         string   `json:"asset_id"`
	Side            string   `json:"side"`
	OriginalSize    string   `json:"original_size"`
	SizeMatched     string   `json:"size_matched"`
	Price           string   `json:"price"`
	AssociateTrades []string `json:"associate_trades"`
	Outcome         string   `json:"outcome"`
	CreatedAt       int64    `json:"created_at"`
	Expiration      string   `json:"expiration"`
	OrderType       string   `json:"order_type"`
}

// TradeParams represents parameters for trade queries
type TradeParams struct {
	ID           *string `json:"id,omitempty"`
	MakerAddress *string `json:"maker_address,omitempty"`
	Market       *string `json:"market,omitempty"`
	AssetID      *string `json:"asset_id,omitempty"`
	Before       *string `json:"before,omitempty"`
	After        *string `json:"after,omitempty"`
}

// OpenOrderParams represents parameters for open order queries
type OpenOrderParams struct {
	ID      *string `json:"id,omitempty"`
	Market  *string `json:"market,omitempty"`
	AssetID *string `json:"asset_id,omitempty"`
}

// MakerOrder represents a maker order in a trade
type MakerOrder struct {
	OrderID       string `json:"order_id"`
	Owner         string `json:"owner"`
	MakerAddress  string `json:"maker_address"`
	MatchedAmount string `json:"matched_amount"`
	Price         string `json:"price"`
	FeeRateBps    string `json:"fee_rate_bps"`
	AssetID       string `json:"asset_id"`
	Outcome       string `json:"outcome"`
	Side          Side   `json:"side"`
}

// Trade represents a trade
type Trade struct {
	ID              string       `json:"id"`
	TakerOrderID    string       `json:"taker_order_id"`
	Market          string       `json:"market"`
	AssetID         string       `json:"asset_id"`
	Side            Side         `json:"side"`
	Size            string       `json:"size"`
	FeeRateBps      string       `json:"fee_rate_bps"`
	Price           string       `json:"price"`
	Status          string       `json:"status"`
	MatchTime       string       `json:"match_time"`
	LastUpdate      string       `json:"last_update"`
	Outcome         string       `json:"outcome"`
	BucketIndex     int          `json:"bucket_index"`
	Owner           string       `json:"owner"`
	MakerAddress    string       `json:"maker_address"`
	MakerOrders     []MakerOrder `json:"maker_orders"`
	TransactionHash string       `json:"transaction_hash"`
	TraderSide      string       `json:"trader_side"` // TAKER or MAKER
}

// MarketPrice represents a market price at a timestamp
type MarketPrice struct {
	Timestamp int64   `json:"t"`
	Price     float64 `json:"p"`
}

// PriceHistoryFilterParams represents parameters for price history queries
type PriceHistoryFilterParams struct {
	Market   *string               `json:"market,omitempty"`
	StartTs  *int64                `json:"startTs,omitempty"`
	EndTs    *int64                `json:"endTs,omitempty"`
	Fidelity *int                  `json:"fidelity,omitempty"`
	Interval *PriceHistoryInterval `json:"interval,omitempty"`
}

// DropNotificationParams represents parameters for dropping notifications
type DropNotificationParams struct {
	IDs []string `json:"ids"`
}

// Notification represents a notification
type Notification struct {
	Type    int         `json:"type"`
	Owner   string      `json:"owner"`
	Payload interface{} `json:"payload"`
}

// OrderMarketCancelParams represents parameters for canceling market orders
type OrderMarketCancelParams struct {
	Market  *string `json:"market,omitempty"`
	AssetID *string `json:"asset_id,omitempty"`
}

// OrderBookSummary represents an order book summary
type OrderBookSummary struct {
	Market         string         `json:"market"`
	AssetID        string         `json:"asset_id"`
	Timestamp      string         `json:"timestamp"`
	Bids           []OrderSummary `json:"bids"`
	Asks           []OrderSummary `json:"asks"`
	MinOrderSize   string         `json:"min_order_size"`
	TickSize       string         `json:"tick_size"`
	NegRisk        bool           `json:"neg_risk"`
	LastTradePrice string         `json:"last_trade_price"`
	Hash           string         `json:"hash"`
}

// OrderSummary represents a price level in the order book
type OrderSummary struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

// BalanceAllowanceParams represents parameters for balance/allowance queries
type BalanceAllowanceParams struct {
	AssetType AssetType `json:"asset_type"`
	TokenID   *string   `json:"token_id,omitempty"`
}

// BalanceAllowanceResponse represents balance and allowance
type BalanceAllowanceResponse struct {
	Balance   string `json:"balance"`
	Allowance string `json:"allowance"`
}

// OrderScoringParams represents parameters for order scoring
type OrderScoringParams struct {
	OrderID string `json:"order_id"`
}

// OrderScoring represents order scoring
type OrderScoring struct {
	Scoring bool `json:"scoring"`
}

// OrdersScoringParams represents parameters for multiple order scoring
type OrdersScoringParams struct {
	OrderIDs []string `json:"orderIds"`
}

// OrdersScoring represents scoring for multiple orders
type OrdersScoring map[string]bool

// CreateOrderOptions represents options for creating an order
type CreateOrderOptions struct {
	TickSize TickSize `json:"tickSize"`
	NegRisk  *bool    `json:"negRisk,omitempty"`
}

// RoundConfig represents rounding configuration
type RoundConfig struct {
	Price  int `json:"price"`
	Size   int `json:"size"`
	Amount int `json:"amount"`
}

// PaginationPayload represents a paginated response
type PaginationPayload struct {
	Limit      int           `json:"limit"`
	Count      int           `json:"count"`
	NextCursor string        `json:"next_cursor"`
	Data       []interface{} `json:"data"`
}

// MarketTradeEvent represents a market trade event
type MarketTradeEvent struct {
	EventType string `json:"event_type"`
	Market    struct {
		ConditionID string `json:"condition_id"`
		AssetID     string `json:"asset_id"`
		Question    string `json:"question"`
		Icon        string `json:"icon"`
		Slug        string `json:"slug"`
	} `json:"market"`
	User struct {
		Address                  string `json:"address"`
		Username                 string `json:"username"`
		ProfilePicture           string `json:"profile_picture"`
		OptimizedProfilePicture  string `json:"optimized_profile_picture"`
		Pseudonym                string `json:"pseudonym"`
	} `json:"user"`
	Side            Side   `json:"side"`
	Size            string `json:"size"`
	FeeRateBps      string `json:"fee_rate_bps"`
	Price           string `json:"price"`
	Outcome         string `json:"outcome"`
	OutcomeIndex    int    `json:"outcome_index"`
	TransactionHash string `json:"transaction_hash"`
	Timestamp       string `json:"timestamp"`
}

// BookParams represents parameters for order book queries
type BookParams struct {
	TokenID string `json:"token_id"`
	Side    Side   `json:"side"`
}

// BuilderApiKey represents builder API key credentials
type BuilderApiKey struct {
	Key        string `json:"key"`
	Secret     string `json:"secret"`
	Passphrase string `json:"passphrase"`
}

// BuilderApiKeyResponse represents builder API key response
type BuilderApiKeyResponse struct {
	Key       string     `json:"key"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}

// BuilderTrade represents a builder trade
type BuilderTrade struct {
	ID              string  `json:"id"`
	TradeType       string  `json:"tradeType"`
	TakerOrderHash  string  `json:"takerOrderHash"`
	Builder         string  `json:"builder"`
	Market          string  `json:"market"`
	AssetID         string  `json:"assetId"`
	Side            string  `json:"side"`
	Size            string  `json:"size"`
	SizeUsdc        string  `json:"sizeUsdc"`
	Price           string  `json:"price"`
	Status          string  `json:"status"`
	Outcome         string  `json:"outcome"`
	OutcomeIndex    int     `json:"outcomeIndex"`
	Owner           string  `json:"owner"`
	Maker           string  `json:"maker"`
	TransactionHash string  `json:"transactionHash"`
	MatchTime       string  `json:"matchTime"`
	BucketIndex     int     `json:"bucketIndex"`
	Fee             string  `json:"fee"`
	FeeUsdc         string  `json:"feeUsdc"`
	ErrMsg          *string `json:"err_msg,omitempty"`
	CreatedAt       *string `json:"createdAt,omitempty"`
	UpdatedAt       *string `json:"updatedAt,omitempty"`
}

// HeartbeatResponse represents a heartbeat response
type HeartbeatResponse struct {
	HeartbeatID string  `json:"heartbeat_id"`
	Error       *string `json:"error,omitempty"`
}
