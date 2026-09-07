package clobclient

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
)

func (c *Client) CreateOrder(ctx context.Context, order UserOrder) (SignedOrderV2, error) {
	if order.OrderType == "" {
		order.OrderType = OrderTypeGTC
	}
	tick, err := c.TickSize(ctx, order.TokenID)
	if err != nil {
		return SignedOrderV2{}, err
	}
	negRisk, err := c.NegRisk(ctx, order.TokenID)
	if err != nil {
		return SignedOrderV2{}, err
	}
	return c.signOrder(order, tick, negRisk)
}

// MarketSellOrder derives a FOK/FAK-compatible sell limit from current bid
// depth. It does not submit the order; callers retain explicit control over
// external side effects by passing the returned order to SubmitOrder.
func (c *Client) MarketSellOrder(ctx context.Context, tokenID string, shares float64, orderType OrderType) (UserOrder, error) {
	if tokenID == "" || shares <= 0 || math.IsNaN(shares) || math.IsInf(shares, 0) {
		return UserOrder{}, fmt.Errorf("token ID and positive shares are required")
	}
	if orderType == "" {
		orderType = OrderTypeFOK
	}
	if orderType != OrderTypeFOK && orderType != OrderTypeFAK {
		return UserOrder{}, fmt.Errorf("market sell order type must be FOK or FAK")
	}
	book, err := c.OrderBook(ctx, tokenID)
	if err != nil {
		return UserOrder{}, err
	}
	tickSize, err := c.TickSize(ctx, tokenID)
	if err != nil {
		return UserOrder{}, err
	}
	minShares, err := minimumShares(book)
	if err != nil {
		return UserOrder{}, err
	}
	if shares+1e-9 < minShares {
		return UserOrder{}, fmt.Errorf("shares %.4f are below minimum order size %.4f", shares, minShares)
	}
	bids := append([]BookLevel(nil), book.Bids...)
	sort.Slice(bids, func(i, j int) bool {
		left, _ := strconv.ParseFloat(bids[i].Price, 64)
		right, _ := strconv.ParseFloat(bids[j].Price, 64)
		return left > right
	})
	remaining := shares
	price := 0.0
	available := 0.0
	for _, bid := range bids {
		levelPrice, priceErr := strconv.ParseFloat(bid.Price, 64)
		levelSize, sizeErr := strconv.ParseFloat(bid.Size, 64)
		if priceErr != nil || sizeErr != nil || levelPrice <= 0 || levelSize <= 0 {
			continue
		}
		available += levelSize
		price = levelPrice
		remaining -= levelSize
		if remaining <= 0 {
			break
		}
	}
	if remaining > 1e-9 || price <= 0 {
		return UserOrder{}, fmt.Errorf("insufficient bid liquidity: need %.4f shares, available %.4f", shares, available)
	}
	price = math.Floor(price/tickSize) * tickSize
	if price <= 0 || price >= 1 {
		return UserOrder{}, fmt.Errorf("derived invalid sell price %.8f", price)
	}
	return UserOrder{TokenID: tokenID, Side: SideSell, Price: price, Shares: shares, OrderType: orderType}, nil
}

// MarketBuyOrder derives a FOK/FAK-compatible buy limit from current ask
// depth. maxNotional is the maximum USDC amount to spend. The returned order
// is not submitted automatically.
func (c *Client) MarketBuyOrder(ctx context.Context, tokenID string, maxNotional float64, orderType OrderType) (UserOrder, error) {
	if tokenID == "" || maxNotional <= 0 || math.IsNaN(maxNotional) || math.IsInf(maxNotional, 0) {
		return UserOrder{}, fmt.Errorf("token ID and positive maximum notional are required")
	}
	if orderType == "" {
		orderType = OrderTypeFOK
	}
	if orderType != OrderTypeFOK && orderType != OrderTypeFAK {
		return UserOrder{}, fmt.Errorf("market buy order type must be FOK or FAK")
	}
	book, err := c.OrderBook(ctx, tokenID)
	if err != nil {
		return UserOrder{}, err
	}
	tickSize, err := c.TickSize(ctx, tokenID)
	if err != nil {
		return UserOrder{}, err
	}
	minShares, err := minimumShares(book)
	if err != nil {
		return UserOrder{}, err
	}
	asks := append([]BookLevel(nil), book.Asks...)
	sort.Slice(asks, func(i, j int) bool {
		left, _ := strconv.ParseFloat(asks[i].Price, 64)
		right, _ := strconv.ParseFloat(asks[j].Price, 64)
		return left < right
	})
	remaining := maxNotional
	price := 0.0
	available := 0.0
	for _, ask := range asks {
		levelPrice, priceErr := strconv.ParseFloat(ask.Price, 64)
		levelSize, sizeErr := strconv.ParseFloat(ask.Size, 64)
		if priceErr != nil || sizeErr != nil || levelPrice <= 0 || levelSize <= 0 {
			continue
		}
		available += levelPrice * levelSize
		price = levelPrice
		remaining -= levelPrice * levelSize
		if remaining <= 0 {
			break
		}
	}
	if remaining > 1e-9 || price <= 0 {
		return UserOrder{}, fmt.Errorf("insufficient ask liquidity: need %.4f USDC, available %.4f", maxNotional, available)
	}
	price = math.Ceil(price/tickSize) * tickSize
	// marketBuyShareScale floors the derived share count to four decimal places
	// so the resulting order stays within the exchange's minimum size and
	// rounding conventions.
	const marketBuyShareScale = 10_000
	shares := math.Floor((maxNotional/price)*marketBuyShareScale) / marketBuyShareScale
	if price <= 0 || price >= 1 || shares <= 0 {
		return UserOrder{}, fmt.Errorf("derived invalid buy order price %.8f shares %.8f", price, shares)
	}
	if shares+1e-9 < minShares {
		return UserOrder{}, fmt.Errorf("derived shares %.4f are below minimum order size %.4f", shares, minShares)
	}
	return UserOrder{TokenID: tokenID, Side: SideBuy, Price: price, Shares: shares, OrderType: orderType}, nil
}

func (c *Client) SubmitOrder(ctx context.Context, order UserOrder) (*OrderResponse, error) {
	signed, err := c.CreateOrder(ctx, order)
	if err != nil {
		return nil, err
	}
	return c.SubmitSignedOrder(ctx, signed, order.OrderType, order.PostOnly)
}

// SubmitSignedOrder submits an already signed order. Callers that need
// crash-safe recovery can persist the signed order before this side effect.
func (c *Client) SubmitSignedOrder(ctx context.Context, signed SignedOrderV2, orderType OrderType, postOnly bool) (*OrderResponse, error) {
	if c.cfg.Credentials == nil {
		return nil, fmt.Errorf("API credentials are required to submit an order")
	}
	if signed.TokenID == "" || signed.Signature == "" {
		return nil, fmt.Errorf("signed order token ID and signature are required")
	}
	if orderType == "" {
		orderType = OrderTypeGTC
	}
	payload := struct {
		DeferExec bool          `json:"deferExec"`
		Order     SignedOrderV2 `json:"order"`
		Owner     string        `json:"owner"`
		OrderType OrderType     `json:"orderType"`
		PostOnly  bool          `json:"postOnly"`
	}{Order: signed, Owner: c.cfg.Credentials.APIKey, OrderType: orderType, PostOnly: postOnly}
	var out OrderResponse
	err := c.transport.Do(ctx, transport.Request{Method: http.MethodPost, Path: "/order", Body: payload, Headers: c.l2Headers, Retryable: false}, &out)
	if err != nil {
		return nil, err
	}
	if !out.Success {
		return &out, &OrderRejectedError{Message: out.ErrorMsg}
	}
	return &out, nil
}

func minimumShares(book *OrderBook) (float64, error) {
	if book == nil || book.MinOrderSize == "" {
		return 0, nil
	}
	value, err := strconv.ParseFloat(book.MinOrderSize, 64)
	if err != nil || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("invalid minimum order size %q", book.MinOrderSize)
	}
	return value, nil
}

func (c *Client) CancelOrder(ctx context.Context, orderID string) error {
	if orderID == "" {
		return fmt.Errorf("order ID is required")
	}
	var out any
	return c.transport.Do(ctx, transport.Request{Method: http.MethodDelete, Path: "/order", Body: map[string]string{"orderID": orderID}, Headers: c.l2Headers}, &out)
}

func (c *Client) CancelOrders(ctx context.Context, orderIDs []string) error {
	if len(orderIDs) == 0 {
		return fmt.Errorf("at least one order ID is required")
	}
	return c.transport.Do(ctx, transport.Request{Method: http.MethodDelete, Path: "/cancel-orders", Body: map[string][]string{"orderIDs": orderIDs}, Headers: c.l2Headers}, nil)
}

func (c *Client) CancelAll(ctx context.Context) error {
	return c.transport.Do(ctx, transport.Request{Method: http.MethodDelete, Path: "/cancel-all", Headers: c.l2Headers}, nil)
}

func (c *Client) CancelMarketOrders(ctx context.Context, market, tokenID string) error {
	if market == "" && tokenID == "" {
		return fmt.Errorf("market or token ID is required")
	}
	body := map[string]string{}
	if market != "" {
		body["market"] = market
	}
	if tokenID != "" {
		body["asset_id"] = tokenID
	}
	return c.transport.Do(ctx, transport.Request{Method: http.MethodDelete, Path: "/cancel-market-orders", Body: body, Headers: c.l2Headers}, nil)
}

func (c *Client) Order(ctx context.Context, orderID string) (*Order, error) {
	var out Order
	err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/data/order/" + url.PathEscape(orderID), Headers: c.l2Headers}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) OpenOrders(ctx context.Context, cursor string) ([]Order, string, error) {
	query := url.Values{}
	if cursor != "" {
		query.Set("next_cursor", cursor)
	}
	var out struct {
		Data       []Order `json:"data"`
		NextCursor string  `json:"next_cursor"`
	}
	err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/data/orders", Query: query, Headers: c.l2Headers}, &out)
	if err != nil {
		return nil, "", err
	}
	return out.Data, out.NextCursor, nil
}

// AllOpenOrders reads every page of currently open orders. It is intended for
// startup and reconnect reconciliation, not latency-sensitive order handling.
func (c *Client) AllOpenOrders(ctx context.Context) ([]Order, error) {
	var all []Order
	for cursor := ""; ; {
		orders, next, err := c.OpenOrders(ctx, cursor)
		if err != nil {
			return nil, err
		}
		all = append(all, orders...)
		if next == "" || next == cursor {
			return all, nil
		}
		cursor = next
	}
}

func (c *Client) BalanceAllowance(ctx context.Context, assetType, tokenID string) (*BalanceAllowance, error) {
	query := url.Values{"asset_type": []string{assetType}, "signature_type": []string{strconv.Itoa(int(c.cfg.SignatureType))}}
	if tokenID != "" {
		query.Set("token_id", tokenID)
	}
	var out BalanceAllowance
	err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/balance-allowance", Query: query, Headers: c.l2Headers}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpdateBalanceAllowance(ctx context.Context, assetType, tokenID string) (*BalanceAllowance, error) {
	query := url.Values{"asset_type": []string{assetType}, "signature_type": []string{strconv.Itoa(int(c.cfg.SignatureType))}}
	if tokenID != "" {
		query.Set("token_id", tokenID)
	}
	var out BalanceAllowance
	err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/balance-allowance/update", Query: query, Headers: c.l2Headers}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Trades(ctx context.Context, nextCursor string) ([]Trade, string, error) {
	query := url.Values{}
	if nextCursor != "" {
		query.Set("next_cursor", nextCursor)
	}
	var out struct {
		Data       []Trade `json:"data"`
		NextCursor string  `json:"next_cursor"`
	}
	err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/data/trades", Query: query, Headers: c.l2Headers}, &out)
	if err != nil {
		return nil, "", err
	}
	return out.Data, out.NextCursor, nil
}

// AllTrades reads every available page of account trades for reconciliation.
func (c *Client) AllTrades(ctx context.Context) ([]Trade, error) {
	var all []Trade
	for cursor := ""; ; {
		trades, next, err := c.Trades(ctx, cursor)
		if err != nil {
			return nil, err
		}
		all = append(all, trades...)
		if next == "" || next == cursor {
			return all, nil
		}
		cursor = next
	}
}

func (c *Client) OrderScoring(ctx context.Context, orderID string) (bool, error) {
	if orderID == "" {
		return false, fmt.Errorf("order ID is required")
	}
	var out struct {
		Scoring bool `json:"scoring"`
	}
	err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/order-scoring", Query: url.Values{"order_id": []string{orderID}}, Headers: c.l2Headers}, &out)
	if err != nil {
		return false, err
	}
	return out.Scoring, nil
}

func (c *Client) Notifications(ctx context.Context) ([]Notification, error) {
	var out []Notification
	query := url.Values{"signature_type": []string{strconv.Itoa(int(c.cfg.SignatureType))}}
	err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/notifications", Query: query, Headers: c.l2Headers}, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) DropNotifications(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	query := url.Values{"signature_type": []string{strconv.Itoa(int(c.cfg.SignatureType))}}
	return c.transport.Do(ctx, transport.Request{Method: http.MethodDelete, Path: "/notifications", Query: query, Body: map[string][]string{"ids": ids}, Headers: c.l2Headers}, nil)
}

func (c *Client) l2Headers(method, path string, body []byte) (http.Header, error) {
	signPath, _, _ := strings.Cut(path, "?")
	raw, err := l2Headers(c.key, c.cfg.Credentials, c.cfg.Now(), method, signPath, body)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header, len(raw))
	for key, value := range raw {
		headers.Set(key, value)
	}
	if c.cfg.Builder != nil {
		return headers, addBuilderHeaders(headers, c.cfg.Builder, c.cfg.Now(), method, signPath, body)
	}
	return headers, nil
}

func addBuilderHeaders(headers http.Header, creds *BuilderCredentials, now time.Time, method, path string, body []byte) error {
	sig, err := hmacSignature(creds.Secret, now.Unix(), method, path, body)
	if err != nil {
		return err
	}
	headers.Set("POLY_BUILDER_API_KEY", creds.APIKey)
	headers.Set("POLY_BUILDER_TIMESTAMP", strconv.FormatInt(now.Unix(), 10))
	headers.Set("POLY_BUILDER_PASSPHRASE", creds.Passphrase)
	headers.Set("POLY_BUILDER_SIGNATURE", sig)
	return nil
}
