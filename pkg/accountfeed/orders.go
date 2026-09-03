package accountfeed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/pkg/contracts"
	"github.com/Cyvadra/polymarket-clob-client/pkg/statemachine"
	"github.com/Cyvadra/polymarket-clob-client/pkg/store"
)

type OrderStore interface {
	store.OrderRepository
}

type OrderConsumer struct {
	store   OrderStore
	now     func() time.Time
	publish contracts.ExecutionEventPublisher
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

func (c *OrderConsumer) Init(context.Context) error  { return nil }
func (c *OrderConsumer) Run(context.Context) error   { return nil }
func (c *OrderConsumer) Close(context.Context) error { return nil }

func (c *OrderConsumer) SetEventPublisher(publisher contracts.ExecutionEventPublisher) {
	c.publish = publisher
}

func (c *OrderConsumer) Consume(ctx context.Context, observation contracts.AccountOrderEvent) error {
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
	if err := contracts.PublishExecutionOrderEvent(c.publish, updated.ExchangeOrderID, string(updated.State), updated.IntentID, updated.MatchedShares, reason, c.now()); err != nil {
		return fmt.Errorf("publish account order event: %w", err)
	}
	if err := PublishTerminalAck(c.publish, updated, reason, c.now()); err != nil {
		return err
	}
	return nil
}

func PublishTerminalAck(publisher contracts.ExecutionEventPublisher, order store.SignedOrderRecord, reason string, occurredAt time.Time) error {
	var status contracts.IntentAckStatus
	switch order.State {
	case statemachine.StateFilled:
		status = contracts.IntentCompleted
	case statemachine.StateCanceled:
		status = contracts.IntentPartial
	case statemachine.StateRejected:
		status = contracts.IntentRejected
	case statemachine.StateExpired:
		status = contracts.IntentExpired
	case statemachine.StateFailed:
		status = contracts.IntentFailed
	default:
		return nil
	}
	if err := contracts.PublishExecutionIntentAck(publisher, contracts.ExecutionIntentAck{
		IntentID: order.IntentID, Status: status, Reason: reason, FilledShares: order.MatchedShares, OccurredAt: occurredAt,
	}); err != nil {
		return fmt.Errorf("publish terminal intent acknowledgement: %w", err)
	}
	return nil
}

func validateOrderEvent(observation contracts.AccountOrderEvent) error {
	if observation.SchemaVersion != "" && observation.SchemaVersion != contracts.SchemaVersionV1 {
		return fmt.Errorf("unsupported order-event schema version %q", observation.SchemaVersion)
	}
	if observation.EventID == "" || observation.ExchangeOrderID == "" || observation.Status == "" {
		return fmt.Errorf("event ID, exchange order ID, and status are required")
	}
	return nil
}
