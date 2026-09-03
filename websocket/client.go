// Package websocket provides reconnecting streams for Polymarket channels.
package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const DefaultMarketURL = "wss://ws-subscriptions-clob.polymarket.com/ws/market"
const DefaultUserURL = "wss://ws-subscriptions-clob.polymarket.com/ws/user"

type Config struct {
	URL               string
	ReconnectDelay    time.Duration
	MaxReconnectDelay time.Duration
	PingInterval      time.Duration
	StaleTimeout      time.Duration
	Dialer            *websocket.Dialer
}

func (c Config) withDefaults() Config {
	if c.ReconnectDelay <= 0 {
		c.ReconnectDelay = time.Second
	}
	if c.MaxReconnectDelay <= 0 {
		c.MaxReconnectDelay = 15 * time.Second
	}
	if c.PingInterval <= 0 {
		c.PingInterval = 20 * time.Second
	}
	if c.StaleTimeout <= 0 {
		c.StaleTimeout = 45 * time.Second
	}
	if c.Dialer == nil {
		c.Dialer = websocket.DefaultDialer
	}
	return c
}

type Event struct {
	Type string
	Data json.RawMessage
	At   time.Time
}

type UserCredentials struct {
	APIKey     string
	Secret     string
	Passphrase string
}

func MarketSubscription(assetIDs ...string) map[string]any {
	return map[string]any{"type": "market", "assets_ids": assetIDs}
}

func UserSubscription(markets []string, credentials UserCredentials) map[string]any {
	return map[string]any{
		"type":    "user",
		"markets": markets,
		"auth": map[string]string{
			"apiKey":     credentials.APIKey,
			"secret":     credentials.Secret,
			"passphrase": credentials.Passphrase,
		},
	}
}

type Client struct {
	cfg       Config
	sub       map[string]any
	events    chan Event
	errors    chan error
	ready     chan struct{}
	readyOnce sync.Once
	closeOnce sync.Once
	done      chan struct{}
}

func New(cfg Config, subscription map[string]any) *Client {
	if cfg.URL == "" {
		if channel, _ := subscription["type"].(string); channel == "user" {
			cfg.URL = DefaultUserURL
		} else {
			cfg.URL = DefaultMarketURL
		}
	}
	cfg = cfg.withDefaults()
	return &Client{cfg: cfg, sub: subscription, events: make(chan Event, 256), errors: make(chan error, 16), ready: make(chan struct{}), done: make(chan struct{})}
}

func (c *Client) Events() <-chan Event   { return c.events }
func (c *Client) Errors() <-chan error   { return c.errors }
func (c *Client) Ready() <-chan struct{} { return c.ready }
func (c *Client) Close()                 { c.closeOnce.Do(func() { close(c.done) }) }

func (c *Client) Run(ctx context.Context) {
	defer close(c.events)
	defer close(c.errors)
	delay := c.cfg.ReconnectDelay
	for {
		if err := c.runOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			c.report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		default:
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.done:
			timer.Stop()
			return
		case <-timer.C:
		}
		delay *= 2
		if delay > c.cfg.MaxReconnectDelay {
			delay = c.cfg.MaxReconnectDelay
		}
	}
}

func (c *Client) runOnce(ctx context.Context) error {
	conn, _, err := c.cfg.Dialer.DialContext(ctx, c.cfg.URL, http.Header{})
	if err != nil {
		return fmt.Errorf("dial websocket: %w", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(c.sub); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	c.readyOnce.Do(func() { close(c.ready) })
	var lastMessage atomic.Int64
	lastMessage.Store(time.Now().UnixNano())
	conn.SetPongHandler(func(string) error { lastMessage.Store(time.Now().UnixNano()); return nil })
	ping := time.NewTicker(c.cfg.PingInterval)
	defer ping.Stop()
	readErr := make(chan error, 1)
	go func() {
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			lastMessage.Store(time.Now().UnixNano())
			var payload map[string]json.RawMessage
			_ = json.Unmarshal(body, &payload)
			var eventType string
			_ = json.Unmarshal(payload["event_type"], &eventType)
			select {
			case c.events <- Event{Type: eventType, Data: append(json.RawMessage(nil), body...), At: time.Now()}:
			case <-c.done:
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return context.Canceled
		case err := <-readErr:
			return fmt.Errorf("read websocket: %w", err)
		case <-ping.C:
			last := time.Unix(0, lastMessage.Load())
			if time.Since(last) > c.cfg.StaleTimeout {
				return fmt.Errorf("websocket stale for %s", time.Since(last).Round(time.Millisecond))
			}
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return fmt.Errorf("ping websocket: %w", err)
			}
		}
	}
}

func (c *Client) report(err error) {
	select {
	case c.errors <- err:
	default:
	}
}
