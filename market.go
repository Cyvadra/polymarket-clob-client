package clobclient

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"

	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
)

const defaultFeeRateBps = 1000

// InvalidateMarketMetadata removes cached signing metadata for tokenID. An
// empty tokenID clears all metadata, which is useful after reconnecting.
func (c *Client) InvalidateMarketMetadata(tokenID string) {
	c.metadata.mu.Lock()
	defer c.metadata.mu.Unlock()
	if tokenID == "" {
		c.metadata.tickSize = map[string]float64{}
		c.metadata.minSize = map[string]float64{}
		c.metadata.negRisk = map[string]bool{}
		c.metadata.feeRate = map[string]int{}
		c.metadata.bookGone = map[string]struct{}{}
		c.metadata.feeSchedule = map[string]FeeSchedule{}
		return
	}
	delete(c.metadata.tickSize, tokenID)
	delete(c.metadata.minSize, tokenID)
	delete(c.metadata.negRisk, tokenID)
	delete(c.metadata.feeRate, tokenID)
	delete(c.metadata.bookGone, tokenID)
}

func (c *Client) OrderBook(ctx context.Context, tokenID string) (*OrderBook, error) {
	if tokenID == "" {
		return nil, fmt.Errorf("token ID is required")
	}
	var out OrderBook
	err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/book", Query: url.Values{"token_id": []string{tokenID}}}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) OrderBooks(ctx context.Context, tokenIDs []string) ([]OrderBook, error) {
	if len(tokenIDs) == 0 {
		return nil, fmt.Errorf("at least one token ID is required")
	}
	body := make([]map[string]string, 0, len(tokenIDs))
	for _, tokenID := range tokenIDs {
		if tokenID == "" {
			return nil, fmt.Errorf("token ID is required")
		}
		body = append(body, map[string]string{"token_id": tokenID})
	}
	var out []OrderBook
	if err := c.transport.Do(ctx, transport.Request{Method: "POST", Path: "/books", Body: body}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) TickSize(ctx context.Context, tokenID string) (float64, error) {
	c.metadata.mu.RLock()
	value, ok := c.metadata.tickSize[tokenID]
	c.metadata.mu.RUnlock()
	if ok {
		return value, nil
	}
	var out struct {
		MinimumTickSize float64 `json:"minimum_tick_size"`
	}
	err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/tick-size", Query: url.Values{"token_id": []string{tokenID}}}, &out)
	if err != nil {
		return 0, err
	}
	if out.MinimumTickSize <= 0 || math.IsNaN(out.MinimumTickSize) {
		return 0, fmt.Errorf("invalid tick size response for %s", tokenID)
	}
	c.metadata.mu.Lock()
	c.metadata.tickSize[tokenID] = out.MinimumTickSize
	c.metadata.mu.Unlock()
	return out.MinimumTickSize, nil
}

// MinOrderSize reports the market's minimum order size in shares. The CLOB
// publishes it on the order book rather than on a metadata endpoint, so the
// book is fetched once per token and the figure cached beside the tick size:
// both are per-market constants and callers ask for them on every order.
//
// A book that omits min_order_size yields 0, meaning "no minimum known", which
// callers treat as no constraint.
func (c *Client) MinOrderSize(ctx context.Context, tokenID string) (float64, error) {
	if tokenID == "" {
		return 0, fmt.Errorf("token ID is required")
	}
	c.metadata.mu.RLock()
	value, ok := c.metadata.minSize[tokenID]
	_, gone := c.metadata.bookGone[tokenID]
	c.metadata.mu.RUnlock()
	if ok {
		return value, nil
	}
	if gone {
		return 0, fmt.Errorf("%w for %s", ErrBookGone, tokenID)
	}
	book, err := c.OrderBook(ctx, tokenID)
	if err != nil {
		if bookGone(err) {
			// The market has settled and its book is gone for good. Cache the
			// verdict so a lane that still holds shares on a settled market
			// costs one round trip rather than one per caller, forever: an
			// uncached 404 here grew executiond's tick from 1s to 60s over
			// nine days and let orders rest past expires_at.
			c.metadata.mu.Lock()
			c.metadata.bookGone[tokenID] = struct{}{}
			c.metadata.mu.Unlock()
			return 0, fmt.Errorf("%w: %w", ErrBookGone, err)
		}
		return 0, err
	}
	size, err := minimumShares(book)
	if err != nil {
		return 0, err
	}
	c.metadata.mu.Lock()
	c.metadata.minSize[tokenID] = size
	c.metadata.mu.Unlock()
	return size, nil
}

// ErrBookGone reports a market whose order book the CLOB no longer serves,
// which is how a settled market presents itself. It is permanent, so callers
// that sweep lanes can retire them instead of retrying.
var ErrBookGone = errors.New("order book no longer exists")

// bookGone reports whether err is the CLOB's permanent "no orderbook" answer.
// Only a 404 qualifies: a timeout or a 5xx is transient and must stay
// uncached, or a blip would strand a live market for the process lifetime.
func bookGone(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	return api.StatusCode == http.StatusNotFound
}

// WarmMarketMetadata loads everything signing and planning an order on
// tokenID needs from the network — tick size, minimum order size and the
// neg-risk flag — concurrently, so the first order on a market does not pay
// for three sequential round trips. Through the production proxy each costs
// 0.26-1.2s, which on 2026-09-15..16 put a median 1.17s between a strategy
// signal and its signed order.
func (c *Client) WarmMarketMetadata(ctx context.Context, tokenID string) error {
	if tokenID == "" {
		return fmt.Errorf("token ID is required")
	}
	errs := make(chan error, 3)
	go func() { _, err := c.TickSize(ctx, tokenID); errs <- err }()
	go func() { _, err := c.MinOrderSize(ctx, tokenID); errs <- err }()
	go func() { _, err := c.NegRisk(ctx, tokenID); errs <- err }()
	var joined error
	for range 3 {
		joined = errors.Join(joined, <-errs)
	}
	return joined
}

func (c *Client) NegRisk(ctx context.Context, tokenID string) (bool, error) {
	c.metadata.mu.RLock()
	value, ok := c.metadata.negRisk[tokenID]
	c.metadata.mu.RUnlock()
	if ok {
		return value, nil
	}
	var out struct {
		NegRisk bool `json:"neg_risk"`
	}
	err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/neg-risk", Query: url.Values{"token_id": []string{tokenID}}}, &out)
	if err != nil {
		return false, err
	}
	c.metadata.mu.Lock()
	c.metadata.negRisk[tokenID] = out.NegRisk
	c.metadata.mu.Unlock()
	return out.NegRisk, nil
}

func (c *Client) FeeRateBps(ctx context.Context, tokenID string) (int, error) {
	c.metadata.mu.RLock()
	value, ok := c.metadata.feeRate[tokenID]
	c.metadata.mu.RUnlock()
	if ok {
		return value, nil
	}
	var out struct {
		BaseFee int `json:"base_fee"`
	}
	err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/fee-rate", Query: url.Values{"token_id": []string{tokenID}}}, &out)
	if err != nil {
		return defaultFeeRateBps, err
	}
	if out.BaseFee < 0 {
		out.BaseFee = defaultFeeRateBps
	}
	c.metadata.mu.Lock()
	c.metadata.feeRate[tokenID] = out.BaseFee
	c.metadata.mu.Unlock()
	return out.BaseFee, nil
}

func (c *Client) Midpoint(ctx context.Context, tokenID string) (float64, error) {
	var out struct {
		Mid string `json:"mid"`
	}
	err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/midpoint", Query: url.Values{"token_id": []string{tokenID}}}, &out)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseFloat(out.Mid, 64)
	if err != nil {
		return 0, fmt.Errorf("parse midpoint: %w", err)
	}
	return value, nil
}

func (c *Client) Price(ctx context.Context, tokenID string, side Side) (float64, error) {
	query := url.Values{"token_id": []string{tokenID}}
	if side != "" {
		query.Set("side", string(side))
	}
	var out struct {
		Price string `json:"price"`
	}
	if err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/price", Query: query}, &out); err != nil {
		return 0, err
	}
	value, err := strconv.ParseFloat(out.Price, 64)
	if err != nil {
		return 0, fmt.Errorf("parse price: %w", err)
	}
	return value, nil
}

func (c *Client) LastTradePrice(ctx context.Context, tokenID string) (float64, error) {
	var out struct {
		Price string `json:"price"`
	}
	if err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/last-trade-price", Query: url.Values{"token_id": []string{tokenID}}}, &out); err != nil {
		return 0, err
	}
	value, err := strconv.ParseFloat(out.Price, 64)
	if err != nil {
		return 0, fmt.Errorf("parse last trade price: %w", err)
	}
	return value, nil
}

func (c *Client) Market(ctx context.Context, conditionID string) (*Market, error) {
	var out Market
	if err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/markets/" + url.PathEscape(conditionID)}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Markets(ctx context.Context, nextCursor string) ([]Market, string, error) {
	query := url.Values{}
	if nextCursor != "" {
		query.Set("next_cursor", nextCursor)
	}
	var out struct {
		Data       []Market `json:"data"`
		NextCursor string   `json:"next_cursor"`
	}
	if err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/markets", Query: query}, &out); err != nil {
		return nil, "", err
	}
	return out.Data, out.NextCursor, nil
}

func (c *Client) SamplingMarkets(ctx context.Context, limit int) ([]Market, string, error) {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Data       []Market `json:"data"`
		NextCursor string   `json:"next_cursor"`
	}
	if err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/sampling-markets", Query: query}, &out); err != nil {
		return nil, "", err
	}
	return out.Data, out.NextCursor, nil
}

func (c *Client) PriceHistory(ctx context.Context, tokenID string, options PriceHistoryOptions) ([]PriceHistoryPoint, error) {
	query := url.Values{"market": []string{tokenID}}
	if options.StartUnix > 0 {
		query.Set("startTs", strconv.FormatInt(options.StartUnix, 10))
	}
	if options.EndUnix > 0 {
		query.Set("endTs", strconv.FormatInt(options.EndUnix, 10))
	}
	if options.Fidelity > 0 {
		query.Set("fidelity", strconv.Itoa(options.Fidelity))
	}
	if options.Interval != "" {
		query.Set("interval", options.Interval)
	}
	var out struct {
		History []PriceHistoryPoint `json:"history"`
	}
	if err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/prices-history", Query: query}, &out); err != nil {
		return nil, err
	}
	return out.History, nil
}
