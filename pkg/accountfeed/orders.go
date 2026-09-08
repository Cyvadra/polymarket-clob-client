package accountfeed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	if err := protocol.PublishExecutionOrderEvent(c.publish, updated.ExchangeOrderID, string(updated.State), updated.IntentID, updated.MatchedShares, reason, c.now()); err != nil {
		return fmt.Errorf("publish account order event: %w", err)
	}
	// Terminal open results carry position identity from the parent intent. A
	// missing intent is abnormal for an order we track; skip rather than error.
	intent, err := c.store.Intent(ctx, updated.IntentID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load order intent for result: %w", err)
	}
	if err := PublishTerminalResult(c.publish, intent, updated, reason, c.now()); err != nil {
		return err
	}
	return nil
}

func PublishTerminalResult(publisher protocol.ExecutionEventPublisher, intent store.OrderIntentRecord, order store.SignedOrderRecord, reason string, occurredAt time.Time) error {
	result, ok := mapping.TerminalResult(order, intent, reason, occurredAt)
	if !ok {
		return nil
	}
	if err := protocol.PublishExecutionOpenResult(publisher, result); err != nil {
		return fmt.Errorf("publish terminal open result: %w", err)
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
