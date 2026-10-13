package clobclient

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
)

func (c *Client) ServerTime(ctx context.Context) (int64, error) {
	return serverTime(ctx, c.transport)
}

func serverTime(ctx context.Context, pool *transport.Client) (int64, error) {
	var raw any
	if err := pool.Do(ctx, transport.Request{Method: "GET", Path: "/time"}, &raw); err != nil {
		return 0, err
	}
	switch value := raw.(type) {
	case float64:
		return int64(value), nil
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse server time: %w", err)
		}
		return parsed, nil
	case map[string]any:
		if field, ok := value["time"]; ok {
			switch v := field.(type) {
			case float64:
				return int64(v), nil
			case string:
				parsed, err := strconv.ParseInt(v, 10, 64)
				if err == nil {
					return parsed, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("unexpected server time response %T", raw)
}

// Warm opens (or confirms) a connection in both the order and the read pool
// with a cheap GET /time, so the next order skips the dial, the proxy CONNECT
// and the TLS handshake. It reports how long each ping took.
func (c *Client) Warm(ctx context.Context) (orders, reads time.Duration, err error) {
	ping := func(pool *transport.Client) (time.Duration, error) {
		start := time.Now()
		_, err := serverTime(ctx, pool)
		return time.Since(start), err
	}
	var orderErr, readErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); orders, orderErr = ping(c.orders) }()
	go func() { defer wg.Done(); reads, readErr = ping(c.transport) }()
	wg.Wait()
	if orderErr != nil {
		orderErr = fmt.Errorf("warm order pool: %w", orderErr)
	}
	if readErr != nil {
		readErr = fmt.Errorf("warm read pool: %w", readErr)
	}
	return orders, reads, errors.Join(orderErr, readErr)
}

// KeepWarm calls Warm every interval until ctx ends, so idle connections are
// never old enough for the proxy, the exchange edge or a NAT to drop them.
// Each ping is bounded by its own timeout, and onPing (optional) sees every
// result; a failed ping is never fatal, the next tick simply redials.
func (c *Client) KeepWarm(ctx context.Context, interval time.Duration, onPing func(orders, reads time.Duration, err error)) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		orders, reads, err := c.Warm(pingCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if onPing != nil {
			onPing(orders, reads, err)
		}
	}
}
