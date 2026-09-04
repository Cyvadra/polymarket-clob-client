package accountfeed

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
}

type UserStream struct {
	cfg     UserStreamConfig
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
	return &UserStream{cfg: cfg, orders: orders, fills: fills}, nil
}

func (s *UserStream) Init(context.Context) error  { return nil }
func (s *UserStream) Close(context.Context) error { return nil }

func (s *UserStream) SetErrorHandler(handler func(error)) { s.onError = handler }

func (s *UserStream) Run(ctx context.Context) error {
	delay := s.cfg.ReconnectDelay
	for ctx.Err() == nil {
		err := s.runOnce(ctx)
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
		delay *= 2
		if delay > s.cfg.MaxReconnect {
			delay = s.cfg.MaxReconnect
		}
	}
	return nil
}

func (s *UserStream) runOnce(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, s.cfg.URL, nil)
	if err != nil {
		return fmt.Errorf("dial user stream: %w", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "user", "auth": map[string]string{
		"apiKey": s.cfg.Credentials.APIKey, "secret": s.cfg.Credentials.Secret, "passphrase": s.cfg.Credentials.Passphrase,
	}}); err != nil {
		return fmt.Errorf("subscribe user stream: %w", err)
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
			return nil
		case err := <-read:
			return fmt.Errorf("read user stream: %w", err)
		case <-pings.C:
			if err := conn.WriteMessage(websocket.TextMessage, []byte("PING")); err != nil {
				return fmt.Errorf("ping user stream: %w", err)
			}
		}
	}
}

func (s *UserStream) consume(ctx context.Context, payload []byte) error {
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
		var event struct {
			ID           string        `json:"id"`
			TakerOrderID string        `json:"taker_order_id"`
			Market       string        `json:"market"`
			AssetID      string        `json:"asset_id"`
			Side         protocol.Side `json:"side"`
			Size         string        `json:"size"`
			Price        string        `json:"price"`
			Outcome      string        `json:"outcome"`
			Status       string        `json:"status"`
			FeeRateBps   string        `json:"fee_rate_bps"`
			TraderSide   string        `json:"trader_side"`
			Timestamp    string        `json:"timestamp"`
			MakerOrders  []struct {
				OrderID       string        `json:"order_id"`
				Owner         string        `json:"owner"`
				MatchedAmount string        `json:"matched_amount"`
				Price         string        `json:"price"`
				AssetID       string        `json:"asset_id"`
				Outcome       string        `json:"outcome"`
				Side          protocol.Side `json:"side"`
			} `json:"maker_orders"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		status := strings.TrimPrefix(strings.ToUpper(event.Status), "TRADE_STATUS_")
		if (status != "MATCHED" && status != "MINED" && status != "CONFIRMED" && status != "FAILED") || event.Outcome == "" {
			return nil
		}
		fills := make([]AccountFill, 0, len(event.MakerOrders)+1)
		if event.TakerOrderID != "" {
			fills = append(fills, AccountFill{SchemaVersion: protocol.SchemaVersionV1, FillID: event.ID, ExchangeOrderID: event.TakerOrderID, MarketID: event.Market, ConditionID: event.Market, TokenID: event.AssetID, Outcome: event.Outcome, Side: event.Side, Shares: event.Size, Price: event.Price, FeeRateBps: event.FeeRateBps, TradeStatus: status, TraderSide: event.TraderSide, ExchangeTime: streamTime(event.Timestamp), ReceivedAt: time.Now().UTC()})
		}
		for _, maker := range event.MakerOrders {
			if maker.OrderID == "" || maker.Outcome == "" {
				continue
			}
			fills = append(fills, AccountFill{SchemaVersion: protocol.SchemaVersionV1, FillID: event.ID + ":" + maker.OrderID, ExchangeOrderID: maker.OrderID, MarketID: event.Market, ConditionID: event.Market, TokenID: maker.AssetID, Outcome: maker.Outcome, Side: maker.Side, Shares: maker.MatchedAmount, Price: maker.Price, FeeRateBps: event.FeeRateBps, TradeStatus: status, TraderSide: "MAKER", ExchangeTime: streamTime(event.Timestamp), ReceivedAt: time.Now().UTC()})
		}
		for _, fill := range fills {
			if _, err := s.fills.Consume(ctx, fill); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

func streamTime(value string) time.Time {
	milliseconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || milliseconds <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(milliseconds).UTC()
}
