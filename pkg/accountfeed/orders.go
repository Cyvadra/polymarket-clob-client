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

// DefaultPriceWait bounds how long a terminal result waits for the fills that
// price it before it is published without an authoritative average_price.
const DefaultPriceWait = 3500 * time.Millisecond

// priceWaitPoll is how often the wait re-reads the order's fill-weighted price.
const priceWaitPoll = 100 * time.Millisecond

type OrderConsumer struct {
	store     store.AccountOrderStore
	now       func() time.Time
	publish   protocol.ExecutionEventPublisher
	priceWait time.Duration
	onError   func(error)
}

func NewOrderConsumer(repository store.AccountOrderStore, now func() time.Time) (*OrderConsumer, error) {
	if repository == nil {
		return nil, fmt.Errorf("order store is required")
	}
	if now == nil {
		now = time.Now
	}
	return &OrderConsumer{store: repository, now: now, priceWait: DefaultPriceWait}, nil
}

func (c *OrderConsumer) SetEventPublisher(publisher protocol.ExecutionEventPublisher) {
	c.publish = publisher
}

// SetErrorHandler receives failures from a result published after the stream
// callback has already returned.
func (c *OrderConsumer) SetErrorHandler(handler func(error)) {
	c.onError = handler
}

// SetPriceWait bounds how long a terminal result waits for its fills to land
// before publishing without average_price. Zero publishes immediately.
func (c *OrderConsumer) SetPriceWait(wait time.Duration) {
	c.priceWait = wait
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
	event, ok := statemachine.EventForOrderStatus(observation.Status, observation.MatchedShares, order.RequestedShares, statemachine.Immediate(string(order.OrderType)))
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
	// The exchange repeats an order message: the same status with the same
	// size_matched arrives several times within milliseconds. Such an
	// observation moves nothing (the store returns the record unchanged, at
	// the same revision), and an order already terminal has reported itself,
	// so republishing would emit a second terminal result for one lane.
	if updated.Revision == order.Revision && statemachine.IsTerminal(order.State) {
		return nil
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
	return c.publishPricedResult(ctx, intent, updated, reason)
}

// publishPricedResult publishes the terminal result, waiting briefly for the
// fills that price it when they have not landed yet. The order event that ends
// an order routinely beats its own trade messages down the user stream, so a
// result published the instant the order goes terminal can carry filled shares
// with no average_price, which books a fill at a cost basis of zero.
//
// The wait runs off the stream goroutine. That goroutine is the one that
// applies the trade messages, so blocking it here would keep the very fills
// being waited on from ever arriving.
func (c *OrderConsumer) publishPricedResult(ctx context.Context, intent store.OrderIntentRecord, order store.SignedOrderRecord, reason string) error {
	price := AveragePrice(ctx, c.store, order)
	if price == "" && c.priceWait > 0 && statemachine.IsTerminal(order.State) && decimal.Positive(order.MatchedShares) {
		go c.awaitPriceAndPublish(context.WithoutCancel(ctx), intent, order, reason)
		return nil
	}
	if price == "" {
		price = PricedResult(ctx, c.store, order)
	}
	return PublishTerminalResult(c.publish, intent, order, reason, price, c.now())
}

func (c *OrderConsumer) awaitPriceAndPublish(ctx context.Context, intent store.OrderIntentRecord, order store.SignedOrderRecord, reason string) {
	deadline, cancel := context.WithTimeout(ctx, c.priceWait)
	defer cancel()
	ticker := time.NewTicker(priceWaitPoll)
	defer ticker.Stop()
	price := ""
	for price == "" {
		select {
		case <-deadline.Done():
			c.publishResult(intent, order, reason, PricedResult(ctx, c.store, order))
			return
		case <-ticker.C:
			price = AveragePrice(deadline, c.store, order)
		}
	}
	c.publishResult(intent, order, reason, price)
}

// publishResult reports a publication failure through the error hook: the
// deferred publication runs off the stream goroutine, so there is no caller
// left to return the error to.
func (c *OrderConsumer) publishResult(intent store.OrderIntentRecord, order store.SignedOrderRecord, reason, price string) {
	if err := PublishTerminalResult(c.publish, intent, order, reason, price, c.now()); err != nil && c.onError != nil {
		c.onError(err)
	}
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

// PricedResult is the price a terminal result reports for an order that
// filled. It prefers the order's recorded fills and falls back to the price
// the order was signed at, which bounds the fill on the side it was placed:
// a result that filled shares but reports no price books that fill at a cost
// basis of zero, which is a worse answer than the bound.
func PricedResult(ctx context.Context, prices store.OrderPriceStore, order store.SignedOrderRecord) string {
	if price := AveragePrice(ctx, prices, order); price != "" {
		return price
	}
	if decimal.Positive(order.MatchedShares) && decimal.Positive(order.Price) {
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
