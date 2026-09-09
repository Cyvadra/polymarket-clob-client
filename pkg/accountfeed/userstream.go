package accountfeed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/gorilla/websocket"
)

const defaultUserStreamURL = "wss://ws-subscriptions-clob.polymarket.com/ws/user"

type UserStreamConfig struct {
	URL            string
	Credentials    clobclient.Credentials
	ReconnectDelay time.Duration
	MaxReconnect   time.Duration
	PingInterval   time.Duration
	// ProxyURL routes the stream through the same egress as the REST client.
	// Without it the dialer falls back to the process proxy environment, so
	// the account stream can leave from a different address than the orders.
	ProxyURL *url.URL
}

type UserStream struct {
	cfg     UserStreamConfig
	dialer  *websocket.Dialer
	orders  *OrderConsumer
	fills   *FillConsumer
	onError func(error)
}

func NewUserStream(cfg UserStreamConfig, orders *OrderConsumer, fills *FillConsumer) (*UserStream, error) {
	if orders == nil || fills == nil {
		return nil, fmt.Errorf("order and fill consumers are required")
	}
	if cfg.Credentials.APIKey == "" || cfg.Credentials.Secret == "" || cfg.Credentials.Passphrase == "" {
		return nil, fmt.Errorf("user stream credentials are required")
	}
	if cfg.URL == "" {
		cfg.URL = defaultUserStreamURL
	}
	if cfg.ReconnectDelay <= 0 {
		cfg.ReconnectDelay = time.Second
	}
	if cfg.MaxReconnect <= 0 {
		cfg.MaxReconnect = 15 * time.Second
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 10 * time.Second
	}
	dialer := *websocket.DefaultDialer
	if cfg.ProxyURL != nil {
		dialer.Proxy = http.ProxyURL(cfg.ProxyURL)
	}
	return &UserStream{cfg: cfg, dialer: &dialer, orders: orders, fills: fills}, nil
}

func (s *UserStream) Init(context.Context) error  { return nil }
func (s *UserStream) Close(context.Context) error { return nil }

func (s *UserStream) SetErrorHandler(handler func(error)) { s.onError = handler }

func (s *UserStream) Run(ctx context.Context) error {
	delay := s.cfg.ReconnectDelay
	for ctx.Err() == nil {
		connected, err := s.runOnce(ctx)
		if err != nil && ctx.Err() == nil && s.onError != nil {
			s.onError(err)
		}
		if ctx.Err() != nil {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if connected {
			delay = s.cfg.ReconnectDelay
			continue
		}
		delay *= 2
		if delay > s.cfg.MaxReconnect {
			delay = s.cfg.MaxReconnect
		}
	}
	return nil
}

func (s *UserStream) runOnce(ctx context.Context) (bool, error) {
	conn, _, err := s.dialer.DialContext(ctx, s.cfg.URL, nil)
	if err != nil {
		return false, fmt.Errorf("dial user stream: %w", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "user", "auth": map[string]string{
		"apiKey": s.cfg.Credentials.APIKey, "secret": s.cfg.Credentials.Secret, "passphrase": s.cfg.Credentials.Passphrase,
	}}); err != nil {
		return false, fmt.Errorf("subscribe user stream: %w", err)
	}
	pings := time.NewTicker(s.cfg.PingInterval)
	defer pings.Stop()
	read := make(chan error, 1)
	go func() {
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				read <- err
				return
			}
			if err := s.consume(ctx, payload); err != nil && s.onError != nil {
				s.onError(err)
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return true, nil
		case err := <-read:
			return true, fmt.Errorf("read user stream: %w", err)
		case <-pings.C:
			if err := conn.WriteMessage(websocket.TextMessage, []byte("PING")); err != nil {
				return true, fmt.Errorf("ping user stream: %w", err)
			}
		}
	}
}

func (s *UserStream) consume(ctx context.Context, payload []byte) error {
	// The stream answers our text PINGs with a bare "PONG" and sends other
	// non-JSON keepalives, so anything that is not a JSON document is not an
	// event and must not be reported as a decode failure.
	if !isJSONPayload(payload) {
		return nil
	}
	var envelope struct {
		EventType string `json:"event_type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode user event: %w", err)
	}
	switch envelope.EventType {
	case "order":
		var event struct {
			ID          string `json:"id"`
			Market      string `json:"market"`
			AssetID     string `json:"asset_id"`
			Status      string `json:"status"`
			SizeMatched string `json:"size_matched"`
			Timestamp   string `json:"timestamp"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		return s.orders.Consume(ctx, AccountOrderEvent{SchemaVersion: protocol.SchemaVersionV1, EventID: event.ID + ":" + event.Status + ":" + event.SizeMatched, ExchangeOrderID: event.ID, ConditionID: event.Market, TokenID: event.AssetID, Status: event.Status, MatchedShares: event.SizeMatched, ReceivedAt: time.Now().UTC(), ExchangeTime: streamTime(event.Timestamp)})
	case "trade":
		var event clobclient.Trade
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		for _, fill := range OwnedFillsFromTrade(event, s.cfg.Credentials.APIKey, time.Now().UTC()) {
			if _, err := s.fills.Consume(ctx, fill); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

// isJSONPayload reports whether the frame looks like a JSON object or array.
func isJSONPayload(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

func streamTime(value string) time.Time {
	milliseconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || milliseconds <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(milliseconds).UTC()
}
