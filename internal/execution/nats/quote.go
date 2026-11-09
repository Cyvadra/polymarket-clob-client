package nats

import (
	"context"
	"fmt"
	"strings"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/natsbus"
)

// QuoteExecutor is the execution surface that consumes quote requests.
// ApplyQuote must not block on the exchange: the lane works the request on
// its own goroutine.
type QuoteExecutor interface {
	ApplyQuote(context.Context, protocol.ExecutionQuoteRequest) error
}

// SubscribeQuote wires strategy.execution.quote to the executor. As for opens,
// a quote for a strategy outside the allowlist belongs to another executiond
// and is dropped silently. A quote publishes nothing on its own, so a refused
// one only reaches onError, for the log.
//
// No lane workers are needed here: the executor queues each lane's requests
// behind its own work and keeps only the newest, so the subscription's serial
// dispatch is never held by an exchange call.
func SubscribeQuote(ctx context.Context, bus Subscriber, execution QuoteExecutor, allowed *Allowlist, onError func(error)) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if bus == nil || execution == nil || allowed == nil {
		return fmt.Errorf("NATS subscriber, executor, and strategy allowlist are required")
	}
	return bus.Subscribe(protocol.SubjectStrategyExecutionQuote, func(_ context.Context, payload []byte) error {
		req, err := natsbus.DecodeJSON[protocol.ExecutionQuoteRequest](payload)
		if err != nil {
			return err
		}
		if strings.TrimSpace(req.Strategy) != "" && !allowed.Allows(req.Strategy) {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		if err := execution.ApplyQuote(context.WithoutCancel(ctx), req); err != nil && onError != nil {
			onError(fmt.Errorf("handle NATS subject %s: %w", protocol.SubjectStrategyExecutionQuote, err))
		}
		return nil
	})
}
