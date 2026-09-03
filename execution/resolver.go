// Package execution resolves asynchronous CLOB order state into actionable fills.
package execution

import (
	"context"
	"fmt"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/internal/poll"
)

type OrderReader interface {
	Order(context.Context, string) (*clobclient.Order, error)
}

type Resolver struct {
	Client       OrderReader
	PollInterval time.Duration
	Now          func() time.Time
}

func (r Resolver) Resolve(ctx context.Context, orderID string, requestedShares float64) (clobclient.Execution, error) {
	if r.Client == nil {
		return clobclient.Execution{}, fmt.Errorf("order reader is required")
	}
	if r.PollInterval <= 0 {
		r.PollInterval = 500 * time.Millisecond
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	var latest clobclient.Execution
	err := poll.Until(ctx, r.PollInterval, func() (bool, error) {
		order, err := r.Client.Order(ctx, orderID)
		if err != nil {
			return false, err
		}
		exec, err := clobclient.ExecutionFromOrder(orderID, order, requestedShares, r.Now())
		if err != nil {
			return false, err
		}
		latest = exec
		return exec.Terminal, nil
	})
	return latest, err
}
