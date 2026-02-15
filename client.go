package clobclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ClobClient is the main client for interacting with the Polymarket CLOB API
type ClobClient struct {
	Host           string
	ChainID        int
	PrivateKey     string
	Creds          *ApiKeyCreds
	SignatureType  SignatureType
	FunderAddress  *string
	OrderBuilder   *OrderBuilder
	HTTPClient     *HTTPClient
	UseServerTime  bool
	BuilderCreds   *BuilderApiKey
	tickSizeCache  map[string]tickSizeCacheEntry
	negRiskCache   map[string]negRiskCacheEntry
}

type tickSizeCacheEntry struct {
	tickSize  TickSize
	timestamp time.Time
}

type negRiskCacheEntry struct {
	negRisk   bool
	timestamp time.Time
}

const (
	cacheTTL = 5 * time.Minute
)

// NewClobClient creates a new CLOB client
func NewClobClient(
	host string,
	chainID int,
	privateKey string,
	creds *ApiKeyCreds,
	signatureType SignatureType,
	funderAddress *string,
) *ClobClient {
	return &ClobClient{
		Host:          host,
		ChainID:       chainID,
		PrivateKey:    privateKey,
		Creds:         creds,
		SignatureType: signatureType,
		FunderAddress: funderAddress,
		OrderBuilder: NewOrderBuilder(
			privateKey,
			chainID,
			signatureType,
			funderAddress,
		),
		HTTPClient:    NewHTTPClient(30*time.Second, true),
		UseServerTime: false,
		tickSizeCache: make(map[string]tickSizeCacheEntry),
		negRiskCache:  make(map[string]negRiskCacheEntry),
	}
}

// API Endpoints
const (
	EndpointTime                = "/time"
	EndpointCreateAPIKey        = "/auth/api-key"
	EndpointDeriveAPIKey        = "/auth/derive-api-key"
	EndpointDeleteAPIKey        = "/auth/api-key"
	EndpointGetAPIKeys          = "/auth/api-keys"
	EndpointCreateReadonlyAPIKey = "/auth/readonly-api-key"
	EndpointPostOrder           = "/order"
	EndpointCancelOrder         = "/order"
	EndpointCancelAll           = "/cancel-all"
	EndpointCancelMarketOrders  = "/cancel-market-orders"
	EndpointCancelOrders        = "/cancel-orders"
	EndpointGetOrder            = "/data/order"
	EndpointGetOpenOrders       = "/data/orders"
	EndpointGetTrades           = "/data/trades"
	EndpointGetOrderBook        = "/book"
	EndpointGetOrderBooks       = "/books"
	EndpointGetMidpoint         = "/midpoint"
	EndpointGetPrice            = "/price"
	EndpointGetLastTradePrice   = "/last-trade-price"
	EndpointGetMarket           = "/market"
	EndpointGetMarkets          = "/markets"
	EndpointGetPricesHistory    = "/prices-history"
	EndpointGetNotifications    = "/notifications"
	EndpointDropNotifications   = "/notifications"
	EndpointGetBalanceAllowance = "/balance-allowance"
	EndpointUpdateBalanceAllowance = "/balance-allowance"
	EndpointGetOrderScoring     = "/order-scoring"
	EndpointGetOrdersScoring    = "/orders-scoring"
	EndpointClosedOnly          = "/closed-only"
)

// GetServerTime returns the server time
func (c *ClobClient) GetServerTime() (int64, error) {
	url := c.Host + EndpointTime

	resp, err := c.HTTPClient.Get(url, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to get server time: %w", err)
	}

	var result struct {
		Time int64 `json:"time"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return 0, fmt.Errorf("failed to parse server time: %w", err)
	}

	return result.Time, nil
}

// CreateAPIKey creates a new API key using L1 authentication
func (c *ClobClient) CreateAPIKey(nonce string) (*ApiKeyCreds, error) {
	url := c.Host + EndpointCreateAPIKey

	// Create L1 headers
	headers, err := CreateL1Headers(c.ChainID, c.PrivateKey, nonce)
	if err != nil {
		return nil, fmt.Errorf("failed to create L1 headers: %w", err)
	}

	resp, err := c.HTTPClient.Post(url, headers, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create API key: %w", err)
	}

	var result ApiKeyRaw
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse API key response: %w", err)
	}

	return &ApiKeyCreds{
		Key:        result.ApiKey,
		Secret:     result.Secret,
		Passphrase: result.Passphrase,
	}, nil
}

// DeriveAPIKey derives an API key using L1 authentication
func (c *ClobClient) DeriveAPIKey(nonce string) (*ApiKeyCreds, error) {
	url := c.Host + EndpointDeriveAPIKey

	// Create L1 headers
	headers, err := CreateL1Headers(c.ChainID, c.PrivateKey, nonce)
	if err != nil {
		return nil, fmt.Errorf("failed to create L1 headers: %w", err)
	}

	resp, err := c.HTTPClient.Get(url, headers)
	if err != nil {
		return nil, fmt.Errorf("failed to derive API key: %w", err)
	}

	var result ApiKeyRaw
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse API key response: %w", err)
	}

	return &ApiKeyCreds{
		Key:        result.ApiKey,
		Secret:     result.Secret,
		Passphrase: result.Passphrase,
	}, nil
}

// CreateOrDeriveAPIKey creates or derives an API key
func (c *ClobClient) CreateOrDeriveAPIKey(nonce string) (*ApiKeyCreds, error) {
	// Try to derive first
	creds, err := c.DeriveAPIKey(nonce)
	if err == nil {
		return creds, nil
	}

	// If derive fails, create a new key
	return c.CreateAPIKey(nonce)
}

// PostOrder posts a signed order to the exchange
func (c *ClobClient) PostOrder(args *PostOrderArgs) (*OrderResponse, error) {
	if c.Creds == nil {
		return nil, fmt.Errorf("API credentials required for posting orders")
	}

	url := c.Host + EndpointPostOrder
	requestPath := EndpointPostOrder

	// Marshal order to JSON for body
	bodyBytes, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal order: %w", err)
	}
	bodyStr := string(bodyBytes)

	// Create L2 headers
	headers, err := CreateL2Headers(
		c.PrivateKey,
		c.Creds,
		http.MethodPost,
		requestPath,
		bodyStr,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create L2 headers: %w", err)
	}

	resp, err := c.HTTPClient.Post(url, headers, args)
	if err != nil {
		return nil, fmt.Errorf("failed to post order: %w", err)
	}

	var result OrderResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse order response: %w", err)
	}

	return &result, nil
}

// CreateOrder creates an order from user input
func (c *ClobClient) CreateOrder(
	userOrder *UserOrder,
	options *CreateOrderOptions,
) (*SignedOrder, error) {
	// Validate price
	if err := ValidatePrice(userOrder.Price, options.TickSize); err != nil {
		return nil, err
	}

	return c.OrderBuilder.BuildOrder(userOrder, options)
}

// CreateAndPostOrder creates and posts an order in one call
func (c *ClobClient) CreateAndPostOrder(
	userOrder *UserOrder,
	options *CreateOrderOptions,
	orderType OrderType,
) (*OrderResponse, error) {
	// Create the order
	signedOrder, err := c.CreateOrder(userOrder, options)
	if err != nil {
		return nil, fmt.Errorf("failed to create order: %w", err)
	}

	// Post the order
	args := &PostOrderArgs{
		Order:     *signedOrder,
		OrderType: orderType,
	}

	return c.PostOrder(args)
}

// CancelOrder cancels an order by ID
func (c *ClobClient) CancelOrder(orderID string) (*OrderResponse, error) {
	if c.Creds == nil {
		return nil, fmt.Errorf("API credentials required for canceling orders")
	}

	url := c.Host + EndpointCancelOrder
	requestPath := EndpointCancelOrder

	body := map[string]string{"orderID": orderID}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal body: %w", err)
	}
	bodyStr := string(bodyBytes)

	headers, err := CreateL2Headers(
		c.PrivateKey,
		c.Creds,
		http.MethodDelete,
		requestPath,
		bodyStr,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create L2 headers: %w", err)
	}

	resp, err := c.HTTPClient.Delete(url, headers, body)
	if err != nil {
		return nil, fmt.Errorf("failed to cancel order: %w", err)
	}

	var result OrderResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &result, nil
}

// CancelAll cancels all open orders
func (c *ClobClient) CancelAll() error {
	if c.Creds == nil {
		return fmt.Errorf("API credentials required for canceling orders")
	}

	url := c.Host + EndpointCancelAll
	requestPath := EndpointCancelAll

	headers, err := CreateL2Headers(
		c.PrivateKey,
		c.Creds,
		http.MethodDelete,
		requestPath,
		"",
	)
	if err != nil {
		return fmt.Errorf("failed to create L2 headers: %w", err)
	}

	_, err = c.HTTPClient.Delete(url, headers, nil)
	if err != nil {
		return fmt.Errorf("failed to cancel all orders: %w", err)
	}

	return nil
}

// CancelMarketOrders cancels all orders for a specific market or asset
func (c *ClobClient) CancelMarketOrders(params *OrderMarketCancelParams) error {
	if c.Creds == nil {
		return fmt.Errorf("API credentials required for canceling orders")
	}

	url := c.Host + EndpointCancelMarketOrders
	requestPath := EndpointCancelMarketOrders

	bodyBytes, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("failed to marshal params: %w", err)
	}
	bodyStr := string(bodyBytes)

	headers, err := CreateL2Headers(
		c.PrivateKey,
		c.Creds,
		http.MethodDelete,
		requestPath,
		bodyStr,
	)
	if err != nil {
		return fmt.Errorf("failed to create L2 headers: %w", err)
	}

	_, err = c.HTTPClient.Delete(url, headers, params)
	if err != nil {
		return fmt.Errorf("failed to cancel market orders: %w", err)
	}

	return nil
}

// GetOpenOrders retrieves open orders
func (c *ClobClient) GetOpenOrders(params *OpenOrderParams) ([]OpenOrder, error) {
	if c.Creds == nil {
		return nil, fmt.Errorf("API credentials required")
	}

	url := c.Host + EndpointGetOpenOrders
	requestPath := EndpointGetOpenOrders

	// Build query parameters
	queryParams := buildQueryParams(params)
	if queryParams != "" {
		url += "?" + queryParams
		requestPath += "?" + queryParams
	}

	headers, err := CreateL2Headers(
		c.PrivateKey,
		c.Creds,
		http.MethodGet,
		requestPath,
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create L2 headers: %w", err)
	}

	resp, err := c.HTTPClient.Get(url, headers)
	if err != nil {
		return nil, fmt.Errorf("failed to get open orders: %w", err)
	}

	var orders []OpenOrder
	if err := json.Unmarshal(resp, &orders); err != nil {
		return nil, fmt.Errorf("failed to parse orders: %w", err)
	}

	return orders, nil
}

// GetTrades retrieves trades
func (c *ClobClient) GetTrades(params *TradeParams) ([]Trade, error) {
	if c.Creds == nil {
		return nil, fmt.Errorf("API credentials required")
	}

	url := c.Host + EndpointGetTrades
	requestPath := EndpointGetTrades

	// Build query parameters
	queryParams := buildQueryParams(params)
	if queryParams != "" {
		url += "?" + queryParams
		requestPath += "?" + queryParams
	}

	headers, err := CreateL2Headers(
		c.PrivateKey,
		c.Creds,
		http.MethodGet,
		requestPath,
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create L2 headers: %w", err)
	}

	resp, err := c.HTTPClient.Get(url, headers)
	if err != nil {
		return nil, fmt.Errorf("failed to get trades: %w", err)
	}

	var trades []Trade
	if err := json.Unmarshal(resp, &trades); err != nil {
		return nil, fmt.Errorf("failed to parse trades: %w", err)
	}

	return trades, nil
}

// GetOrderBook retrieves the order book for a token
func (c *ClobClient) GetOrderBook(tokenID string) (*OrderBookSummary, error) {
	url := fmt.Sprintf("%s%s?token_id=%s", c.Host, EndpointGetOrderBook, tokenID)

	resp, err := c.HTTPClient.Get(url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get order book: %w", err)
	}

	var book OrderBookSummary
	if err := json.Unmarshal(resp, &book); err != nil {
		return nil, fmt.Errorf("failed to parse order book: %w", err)
	}

	return &book, nil
}

// GetPrice retrieves the mid price for a token
func (c *ClobClient) GetPrice(tokenID string, side *Side) (float64, error) {
	url := fmt.Sprintf("%s%s?token_id=%s", c.Host, EndpointGetPrice, tokenID)
	if side != nil {
		url += "&side=" + string(*side)
	}

	resp, err := c.HTTPClient.Get(url, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to get price: %w", err)
	}

	var result struct {
		Price string `json:"price"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return 0, fmt.Errorf("failed to parse price: %w", err)
	}

	price, err := strconv.ParseFloat(result.Price, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse price value: %w", err)
	}

	return price, nil
}

// GetMidpoint retrieves the midpoint price for a token
func (c *ClobClient) GetMidpoint(tokenID string) (float64, error) {
	url := fmt.Sprintf("%s%s?token_id=%s", c.Host, EndpointGetMidpoint, tokenID)

	resp, err := c.HTTPClient.Get(url, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to get midpoint: %w", err)
	}

	var result struct {
		Mid string `json:"mid"`
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return 0, fmt.Errorf("failed to parse midpoint: %w", err)
	}

	mid, err := strconv.ParseFloat(result.Mid, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse midpoint value: %w", err)
	}

	return mid, nil
}

// GetBalanceAllowance retrieves balance and allowance for an asset
func (c *ClobClient) GetBalanceAllowance(params *BalanceAllowanceParams) (*BalanceAllowanceResponse, error) {
	if c.Creds == nil {
		return nil, fmt.Errorf("API credentials required")
	}

	url := c.Host + EndpointGetBalanceAllowance
	requestPath := EndpointGetBalanceAllowance

	queryParams := buildQueryParams(params)
	if queryParams != "" {
		url += "?" + queryParams
		requestPath += "?" + queryParams
	}

	headers, err := CreateL2Headers(
		c.PrivateKey,
		c.Creds,
		http.MethodGet,
		requestPath,
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create L2 headers: %w", err)
	}

	resp, err := c.HTTPClient.Get(url, headers)
	if err != nil {
		return nil, fmt.Errorf("failed to get balance allowance: %w", err)
	}

	var result BalanceAllowanceResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse balance allowance: %w", err)
	}

	return &result, nil
}

// Helper function to build query parameters
func buildQueryParams(params interface{}) string {
	if params == nil {
		return ""
	}

	// Convert to JSON and then to map
	data, err := json.Marshal(params)
	if err != nil {
		return ""
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}

	// Build query string
	var parts []string
	for key, value := range m {
		if value != nil {
			parts = append(parts, fmt.Sprintf("%s=%v", key, value))
		}
	}

	return strings.Join(parts, "&")
}
