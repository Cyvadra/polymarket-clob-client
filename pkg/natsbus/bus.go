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

// ReplyHandler receives the request payload together with the reply subject
// the requester expects the response on (from Msg.Reply).
type ReplyHandler func(context.Context, string, []byte) error

type Bus struct {
	config Config
	conn   *nats.Conn
	mu     sync.Mutex
	subs   []*nats.Subscription
	ctx    context.Context
	done   chan error
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
	options = append(options,
		nats.ErrorHandler(func(_ *nats.Conn, subscription *nats.Subscription, err error) {
			if b.config.OnHandlerError != nil {
				subject := ""
				if subscription != nil {
					subject = subscription.Subject
				}
				b.config.OnHandlerError(fmt.Errorf("NATS async error on subject %s: %w", subject, err))
			}
		}),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil && b.config.OnHandlerError != nil {
				b.config.OnHandlerError(fmt.Errorf("NATS disconnected: %w", err))
			}
		}),
		nats.ClosedHandler(func(_ *nats.Conn) {
			b.finish(fmt.Errorf("NATS connection closed"))
		}),
	)
	conn, err := nats.Connect(b.config.URL, options...)
	if err != nil {
		return fmt.Errorf("connect NATS: %w", err)
	}
	b.conn = conn
	b.ctx = ctx
	b.done = make(chan error, 1)
	return nil
}

func (b *Bus) Run(ctx context.Context) error {
	b.mu.Lock()
	done := b.done
	b.mu.Unlock()
	if done == nil {
		return fmt.Errorf("NATS bus is not initialized")
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
}

// Close stops delivery and drains the connection. Draining happens outside the
// bus mutex because an in-flight handler may still be publishing, and that
// publish needs the same mutex.
func (b *Bus) Close(ctx context.Context) error {
	b.mu.Lock()
	subs := b.subs
	conn := b.conn
	b.subs, b.conn, b.ctx, b.done = nil, nil, nil, nil
	b.mu.Unlock()

	var closeErr error
	for _, subscription := range subs {
		if err := subscription.Unsubscribe(); err != nil {
			closeErr = fmt.Errorf("unsubscribe NATS: %w", err)
		}
	}
	if conn == nil {
		return closeErr
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_ = conn.Drain()
	}()
	select {
	case <-drained:
	case <-ctx.Done():
	}
	conn.Close()
	return closeErr
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
	conn := b.conn
	b.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("NATS bus is not initialized")
	}
	if err := conn.Publish(subject, payload); err != nil {
		return fmt.Errorf("publish NATS payload: %w", err)
	}
	return nil
}

func (b *Bus) finish(err error) {
	b.mu.Lock()
	done := b.done
	b.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case done <- err:
	default:
	}
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

// SubscribeReply registers a request/reply subscription. The handler receives
// the reply subject from Msg.Reply and is responsible for publishing the
// response back on it (for example with PublishJSON).
func (b *Bus) SubscribeReply(subject string, handler ReplyHandler) error {
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
		if err := handler(ctx, message.Reply, message.Data); err != nil && b.config.OnHandlerError != nil {
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
