// Package natsbus contains the shared NATS transport boundary for runtime modules.
package natsbus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

type Config struct {
	URL            string
	Name           string
	ConnectTimeout time.Duration
	OnHandlerError func(error)
}

type Handler func(context.Context, []byte) error

type Bus struct {
	config Config
	conn   *nats.Conn
	mu     sync.Mutex
	subs   []*nats.Subscription
	ctx    context.Context
}

func New(cfg Config) (*Bus, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("NATS URL is required")
	}
	return &Bus{config: cfg}, nil
}

func (b *Bus) Init(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ctx == nil {
		return fmt.Errorf("NATS context is required")
	}
	if b.conn != nil {
		return nil
	}
	options := []nats.Option{}
	if b.config.Name != "" {
		options = append(options, nats.Name(b.config.Name))
	}
	if b.config.ConnectTimeout > 0 {
		options = append(options, nats.Timeout(b.config.ConnectTimeout))
	}
	conn, err := nats.Connect(b.config.URL, options...)
	if err != nil {
		return fmt.Errorf("connect NATS: %w", err)
	}
	b.conn = conn
	b.ctx = ctx
	return nil
}

func (b *Bus) Run(ctx context.Context) error {
	if err := b.Init(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func (b *Bus) Close(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, subscription := range b.subs {
		if err := subscription.Unsubscribe(); err != nil {
			return fmt.Errorf("unsubscribe NATS: %w", err)
		}
	}
	b.subs = nil
	if b.conn != nil {
		b.conn.Drain()
		b.conn.Close()
		b.conn = nil
	}
	b.ctx = nil
	return nil
}

func (b *Bus) PublishJSON(subject string, value any) error {
	if subject == "" {
		return fmt.Errorf("NATS subject is required")
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal NATS payload: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return fmt.Errorf("NATS bus is not initialized")
	}
	if err := b.conn.Publish(subject, payload); err != nil {
		return fmt.Errorf("publish NATS payload: %w", err)
	}
	return nil
}

func (b *Bus) Subscribe(subject string, handler Handler) error {
	if subject == "" || handler == nil {
		return fmt.Errorf("NATS subject and handler are required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return fmt.Errorf("NATS bus is not initialized")
	}
	subscription, err := b.conn.Subscribe(subject, func(message *nats.Msg) {
		ctx := b.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if err := handler(ctx, message.Data); err != nil && b.config.OnHandlerError != nil {
			b.config.OnHandlerError(fmt.Errorf("handle NATS subject %s: %w", subject, err))
		}
	})
	if err != nil {
		return fmt.Errorf("subscribe NATS subject %s: %w", subject, err)
	}
	b.subs = append(b.subs, subscription)
	return nil
}

func DecodeJSON[T any](payload []byte) (T, error) {
	var value T
	if err := json.Unmarshal(payload, &value); err != nil {
		return value, fmt.Errorf("decode NATS JSON: %w", err)
	}
	return value, nil
}
