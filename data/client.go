// Package data provides a context-aware client for Polymarket's Data API.
package data

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
)

const DefaultHost = "https://data-api.polymarket.com"

type Config struct {
	Host       string
	HTTPClient *http.Client
	Timeout    time.Duration
	QPS        int
}

type Client struct {
	transport *transport.Client
}

type Position struct {
	Asset        string  `json:"asset"`
	ConditionID  string  `json:"conditionId"`
	Size         float64 `json:"size"`
	AvgPrice     float64 `json:"avgPrice"`
	CurrentValue float64 `json:"currentValue"`
	Redeemable   bool    `json:"redeemable"`
	Mergeable    bool    `json:"mergeable"`
}

type Activity struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Asset     string `json:"asset"`
	Condition string `json:"conditionId"`
	Side      string `json:"side"`
	Size      string `json:"size"`
	Price     string `json:"price"`
	Timestamp int64  `json:"timestamp"`
}

func New(cfg Config) (*Client, error) {
	if cfg.Host == "" {
		cfg.Host = DefaultHost
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	parsed, err := url.Parse(cfg.Host)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Data API host %q", cfg.Host)
	}
	return &Client{transport: transport.New(strings.TrimRight(cfg.Host, "/"), cfg.HTTPClient, cfg.Timeout, 3, 250*time.Millisecond, cfg.QPS)}, nil
}

func (c *Client) Positions(ctx context.Context, wallet string, limit, offset int) ([]Position, error) {
	if wallet == "" {
		return nil, fmt.Errorf("wallet is required")
	}
	query := url.Values{"user": []string{wallet}}
	if limit > 0 {
		query.Set("limit", fmt.Sprint(limit))
	}
	if offset > 0 {
		query.Set("offset", fmt.Sprint(offset))
	}
	var positions []Position
	if err := c.transport.Do(ctx, transport.Request{Method: http.MethodGet, Path: "/positions", Query: query}, &positions); err != nil {
		return nil, err
	}
	return positions, nil
}

func (c *Client) Activity(ctx context.Context, wallet, activityType string, limit, offset int) ([]Activity, error) {
	if wallet == "" {
		return nil, fmt.Errorf("wallet is required")
	}
	query := url.Values{"user": []string{wallet}}
	if activityType != "" {
		query.Set("type", activityType)
	}
	if limit > 0 {
		query.Set("limit", fmt.Sprint(limit))
	}
	if offset > 0 {
		query.Set("offset", fmt.Sprint(offset))
	}
	var activities []Activity
	if err := c.transport.Do(ctx, transport.Request{Method: http.MethodGet, Path: "/activity", Query: query}, &activities); err != nil {
		return nil, err
	}
	return activities, nil
}
