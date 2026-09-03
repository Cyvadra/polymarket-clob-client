// Package gamma provides a context-aware client for Polymarket's Gamma API.
package gamma

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
)

const DefaultHost = "https://gamma-api.polymarket.com"

type Config struct {
	Host       string
	HTTPClient *http.Client
	Timeout    time.Duration
	QPS        int
}

type Client struct {
	transport *transport.Client
}

type Event struct {
	ID      string   `json:"id"`
	Slug    string   `json:"slug"`
	Title   string   `json:"title"`
	Active  bool     `json:"active"`
	Closed  bool     `json:"closed"`
	Markets []Market `json:"markets"`
}

type Market struct {
	ID          string `json:"id"`
	ConditionID string `json:"conditionId"`
	Slug        string `json:"slug"`
	Question    string `json:"question"`
	Active      bool   `json:"active"`
	Closed      bool   `json:"closed"`
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
		return nil, fmt.Errorf("invalid Gamma host %q", cfg.Host)
	}
	return &Client{transport: transport.New(strings.TrimRight(cfg.Host, "/"), cfg.HTTPClient, cfg.Timeout, 3, 250*time.Millisecond, cfg.QPS)}, nil
}

func (c *Client) EventBySlug(ctx context.Context, slug string) (*Event, error) {
	if slug == "" {
		return nil, fmt.Errorf("event slug is required")
	}
	var result []Event
	err := c.transport.Do(ctx, transport.Request{Method: http.MethodGet, Path: "/events", Query: url.Values{"slug": []string{slug}}}, &result)
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("event %q not found", slug)
	}
	return &result[0], nil
}

func (c *Client) MarketsByEventSlug(ctx context.Context, slug string) ([]Market, error) {
	event, err := c.EventBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	return event.Markets, nil
}

func (c *Client) MarketBySlug(ctx context.Context, slug string) (*Market, error) {
	if slug == "" {
		return nil, fmt.Errorf("market slug is required")
	}
	var result []Market
	err := c.transport.Do(ctx, transport.Request{Method: http.MethodGet, Path: "/markets", Query: url.Values{"slug": []string{slug}}}, &result)
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("market %q not found", slug)
	}
	return &result[0], nil
}
