package accountfeed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type OrderStore interface {
	store.OrderRepository
}

type OrderConsumer struct {
	store   OrderStore
	now     func() time.Time
	publish protocol.ExecutionEventPublisher
}

func NewOrderConsumer(repository OrderStore, now func() time.Time) (*OrderConsumer, error) {
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
	event, ok := statemachine.EventForOrderObservation(observation.Status, observation.MatchedShares, order.RequestedShares)
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
	if err := PublishTerminalAck(c.publish, updated, reason, c.now()); err != nil {
		return err
	}
	return nil
}

func PublishTerminalAck(publisher protocol.ExecutionEventPublisher, order store.SignedOrderRecord, reason string, occurredAt time.Time) error {
	var status protocol.IntentAckStatus
	switch order.State {
	case statemachine.StateFilled:
		status = protocol.IntentCompleted
	case statemachine.StateCanceled:
		status = protocol.IntentPartial
	case statemachine.StateRejected:
		status = protocol.IntentRejected
	case statemachine.StateExpired:
		status = protocol.IntentExpired
	case statemachine.StateFailed:
		status = protocol.IntentFailed
	default:
		return nil
	}
	if err := protocol.PublishExecutionIntentAck(publisher, protocol.ExecutionIntentAck{
		IntentID: order.IntentID, Status: status, Reason: reason, FilledShares: order.MatchedShares, OccurredAt: occurredAt,
	}); err != nil {
		return fmt.Errorf("publish terminal intent acknowledgement: %w", err)
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
