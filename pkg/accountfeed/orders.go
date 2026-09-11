package accountfeed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/decimal"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/mapping"
	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type OrderConsumer struct {
	store   store.AccountOrderStore
	now     func() time.Time
	publish protocol.ExecutionEventPublisher
}

func NewOrderConsumer(repository store.AccountOrderStore, now func() time.Time) (*OrderConsumer, error) {
	if repository == nil {
		return nil, fmt.Errorf("order store is required")
	}
	if now == nil {
		now = time.Now
	}
	return &OrderConsumer{store: repository, now: now}, nil
}

func (c *OrderConsumer) SetEventPublisher(publisher protocol.ExecutionEventPublisher) {
	c.publish = publisher
}

func (c *OrderConsumer) Consume(ctx context.Context, observation AccountOrderEvent) error {
	if err := validateOrderEvent(observation); err != nil {
		return err
	}
	order, err := c.store.OrderByExchangeID(ctx, observation.ExchangeOrderID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lookup observed order: %w", err)
	}
	event, ok := statemachine.EventForOrderObservation(observation.Status, observation.MatchedShares, order.RequestedShares, statemachine.Immediate(string(order.OrderType)))
	if !ok {
		return fmt.Errorf("unsupported account order status %q", observation.Status)
	}
	reason := "account order event " + strings.ToUpper(strings.TrimSpace(observation.Status))
	matchedShares := observation.MatchedShares
	if matchedShares == "" {
		matchedShares = order.MatchedShares
	}
	updated, err := c.store.TransitionOrder(ctx, order, event, matchedShares, observation.ExchangeOrderID, reason)
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("persist account order observation: %w", err)
	}
	// The order event and the terminal result both carry the lane tag from the
	// parent intent, so load it first. A missing intent is abnormal for an
	// order we track; publish the event with no tag and skip the result.
	intent, err := c.store.Intent(ctx, updated.IntentID)
	if errors.Is(err, store.ErrNotFound) {
		intent = store.OrderIntentRecord{}
	} else if err != nil {
		return fmt.Errorf("load order intent for result: %w", err)
	}
	if err := protocol.PublishExecutionOrderEvent(c.publish, updated.ExchangeOrderID, string(updated.State), updated.IntentID, intent.UniqueTag, updated.MatchedShares, reason, c.now()); err != nil {
		return fmt.Errorf("publish account order event: %w", err)
	}
	if intent.IntentID == "" {
		return nil
	}
	if err := PublishTerminalResult(c.publish, intent, updated, reason, AveragePrice(ctx, c.store, updated), c.now()); err != nil {
		return err
	}
	return nil
}

// AveragePrice looks up the recorded fill price of a terminal order for its
// result. It is best effort: fills can land after the order event, and a
// failed lookup must not hold back the result, so either yields "" and the
// result simply omits average_price. When no fill price is on record yet it
// falls back to the order's own limit only for a post-only resting order
// (GTC/GTD): post-only can never cross, so a maker fill is exactly at the
// limit. A plain GTC/GTD without post-only can cross immediately at a better
// price on arrival, and a taker order (FAK/FOK) is priced by the executor
// from the submission response, not here, so neither gets this fallback.
func AveragePrice(ctx context.Context, prices store.OrderPriceStore, order store.SignedOrderRecord) string {
	if prices == nil || order.ExchangeOrderID == "" || !statemachine.IsTerminal(order.State) {
		return ""
	}
	price, err := prices.OrderAveragePrice(ctx, order.ExchangeOrderID)
	if err == nil && price != "" {
		return price
	}
	if order.PostOnly && decimal.Positive(order.MatchedShares) && decimal.Positive(order.Price) && restingOrderType(order.OrderType) {
		return order.Price
	}
	return ""
}

func restingOrderType(tif store.TimeInForce) bool {
	return tif == store.TimeInForceGTC || tif == store.TimeInForceGTD
}

func PublishTerminalResult(publisher protocol.ExecutionEventPublisher, intent store.OrderIntentRecord, order store.SignedOrderRecord, reason, averagePrice string, occurredAt time.Time) error {
	if intent.Kind == store.IntentClose && intent.Status == store.IntentStatusSuperseded {
		return nil
	}
	if result, ok := mapping.TerminalResult(order, intent, reason, averagePrice, occurredAt); ok {
		if err := protocol.PublishExecutionOpenResult(publisher, result); err != nil {
			return fmt.Errorf("publish terminal open result: %w", err)
		}
		return nil
	}
	if result, ok := mapping.TerminalCloseResult(order, intent, reason, averagePrice, occurredAt); ok {
		if err := protocol.PublishExecutionCloseResult(publisher, result); err != nil {
			return fmt.Errorf("publish terminal close result: %w", err)
		}
		return nil
	}
	return nil
}

func validateOrderEvent(observation AccountOrderEvent) error {
	if observation.SchemaVersion != "" && observation.SchemaVersion != protocol.SchemaVersionV1 {
		return fmt.Errorf("unsupported order-event schema version %q", observation.SchemaVersion)
	}
	if observation.EventID == "" || observation.ExchangeOrderID == "" || observation.Status == "" {
		return fmt.Errorf("event ID, exchange order ID, and status are required")
	}
	return nil
}
