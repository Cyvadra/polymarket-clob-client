package clobclient

import (
	"context"
	"fmt"
	"strconv"

	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
)

func (c *Client) ServerTime(ctx context.Context) (int64, error) {
	var raw any
	if err := c.transport.Do(ctx, transport.Request{Method: "GET", Path: "/time"}, &raw); err != nil {
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
