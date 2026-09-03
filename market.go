package clobclient

import (
	"context"
	"fmt"
	"math"
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
		c.metadata.negRisk = map[string]bool{}
		c.metadata.feeRate = map[string]int{}
		return
	}
	delete(c.metadata.tickSize, tokenID)
	delete(c.metadata.negRisk, tokenID)
	delete(c.metadata.feeRate, tokenID)
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
