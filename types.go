package clobclient

import "time"

type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

type OrderType string

const (
	OrderTypeGTC OrderType = "GTC"
	OrderTypeFOK OrderType = "FOK"
	OrderTypeFAK OrderType = "FAK"
	OrderTypeGTD OrderType = "GTD"
)

type SignatureType int

const (
	SignatureTypeEOA        SignatureType = 0
	SignatureTypePolyProxy  SignatureType = 1
	SignatureTypeGnosisSafe SignatureType = 2
	SignatureTypePoly1271   SignatureType = 3
)

const (
	ChainPolygonMainnet int64 = 137
	ChainPolygonAmoy    int64 = 80002
)

type Credentials struct {
	APIKey     string
	Secret     string
	Passphrase string
}

type BuilderCredentials struct {
	APIKey     string
	Secret     string
	Passphrase string
}

type UserOrder struct {
	TokenID       string
	Side          Side
	Price         float64
	Shares        float64
	Expiration    int64
	PostOnly      bool
	OrderType     OrderType
	Builder       string
	BuilderConfig string
}

type SignedOrderV2 struct {
	Salt          int64  `json:"salt"`
	Maker         string `json:"maker"`
	Signer        string `json:"signer"`
	TokenID       string `json:"tokenId"`
	MakerAmount   string `json:"makerAmount"`
	TakerAmount   string `json:"takerAmount"`
	Expiration    string `json:"expiration"`
	Side          string `json:"side"`
	SignatureType int    `json:"signatureType"`
	Timestamp     string `json:"timestamp"`
	Metadata      string `json:"metadata"`
	Builder       string `json:"builder"`
	Signature     string `json:"signature"`
}

type OrderResponse struct {
	Success            bool     `json:"success"`
	ErrorMsg           string   `json:"errorMsg"`
	OrderID            string   `json:"orderID"`
	Status             string   `json:"status"`
	TakingAmount       string   `json:"takingAmount"`
	MakingAmount       string   `json:"makingAmount"`
	TransactionsHashes []string `json:"transactionsHashes"`
}

type Order struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	AssetID      string `json:"asset_id"`
	Side         Side   `json:"side"`
	OriginalSize string `json:"original_size"`
	SizeMatched  string `json:"size_matched"`
	Price        string `json:"price"`
	AvgPrice     string `json:"avg_price"`
	CreatedAt    int64  `json:"created_at"`
}

type BookLevel struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

type OrderBook struct {
	Market       string      `json:"market"`
	AssetID      string      `json:"asset_id"`
	Bids         []BookLevel `json:"bids"`
	Asks         []BookLevel `json:"asks"`
	MinOrderSize string      `json:"min_order_size"`
	TickSize     string      `json:"tick_size"`
	NegRisk      bool        `json:"neg_risk"`
}

type BalanceAllowance struct {
	Balance   string `json:"balance"`
	Allowance string `json:"allowance"`
}

type Market struct {
	ConditionID string  `json:"condition_id"`
	Slug        string  `json:"market_slug"`
	Question    string  `json:"question"`
	Active      bool    `json:"active"`
	Closed      bool    `json:"closed"`
	Tokens      []Token `json:"tokens"`
}

type Token struct {
	TokenID string  `json:"token_id"`
	Outcome string  `json:"outcome"`
	Price   float64 `json:"price"`
}

type Trade struct {
	ID        string `json:"id"`
	AssetID   string `json:"asset_id"`
	Market    string `json:"market"`
	Side      Side   `json:"side"`
	Price     string `json:"price"`
	Size      string `json:"size"`
	Timestamp string `json:"timestamp"`
}

type PriceHistoryPoint struct {
	Timestamp int64   `json:"t"`
	Price     float64 `json:"p"`
}

type PriceHistoryOptions struct {
	StartUnix int64
	EndUnix   int64
	Fidelity  int
	Interval  string
}

type Notification struct {
	ID      string `json:"id"`
	Type    int    `json:"type"`
	Owner   string `json:"owner"`
	Payload any    `json:"payload"`
}

type ExecutionStatus string

const (
	ExecutionLive     ExecutionStatus = "LIVE"
	ExecutionMatched  ExecutionStatus = "MATCHED"
	ExecutionCanceled ExecutionStatus = "CANCELED"
	ExecutionDelayed  ExecutionStatus = "DELAYED"
	ExecutionUnknown  ExecutionStatus = "UNKNOWN"
)

type Execution struct {
	OrderID         string
	Status          ExecutionStatus
	RequestedShares float64
	MatchedShares   float64
	AveragePrice    float64
	Terminal        bool
	UpdatedAt       time.Time
}
