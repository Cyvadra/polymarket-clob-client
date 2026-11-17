package accountfeed

import (
	"context"
	"encoding/json"
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
	store store.AccountOrderStore
	// aggregates is the same store seen as a multi-level reader, nil when it
	// is not one.
	aggregates store.AggregateOrderStore
	now        func() time.Time
	publish    protocol.ExecutionEventPublisher
	priceWait  time.Duration
	onError    func(error)
	onQuote    QuoteFillHook
	pending    *PendingOrders
}

// SetPendingOrders holds events for orders whose row is still committing,
// instead of dropping them.
func (c *OrderConsumer) SetPendingOrders(pending *PendingOrders) {
	c.pending = pending
}

func NewOrderConsumer(repository store.AccountOrderStore, now func() time.Time) (*OrderConsumer, error) {
	if repository == nil {
		return nil, fmt.Errorf("order store is required")
	}
	if now == nil {
		now = time.Now
	}
	aggregates, _ := repository.(store.AggregateOrderStore)
	return &OrderConsumer{store: repository, aggregates: aggregates, now: now, priceWait: DefaultPriceWait}, nil
}

func (c *OrderConsumer) SetEventPublisher(publisher protocol.ExecutionEventPublisher) {
	c.publish = publisher
}

// SetErrorHandler receives failures from a result published after the stream
// callback has already returned.
func (c *OrderConsumer) SetErrorHandler(handler func(error)) {
	c.onError = handler
}

// SetQuoteFillHook is told of every observed change to a quote level, so the
// first fill can end its lane.
func (c *OrderConsumer) SetQuoteFillHook(hook QuoteFillHook) {
	c.onQuote = hook
}

// SetPriceWait bounds how long a terminal result waits for its fills to land
// before publishing without average_price. Zero publishes immediately.
func (c *OrderConsumer) SetPriceWait(wait time.Duration) {
	c.priceWait = wait
}

func (c *OrderConsumer) Consume(ctx context.Context, observation AccountOrderEvent) error {
	return c.consume(ctx, observation, true)
}

// consume applies observation. holdable is false on a replay from
// PendingOrders, which must not queue the event again.
func (c *OrderConsumer) consume(ctx context.Context, observation AccountOrderEvent, holdable bool) error {
	if err := validateOrderEvent(observation); err != nil {
		return err
	}
	order, err := c.store.OrderByExchangeID(ctx, observation.ExchangeOrderID)
	if errors.Is(err, store.ErrNotFound) {
		// An order posted before its row committed: replay the event once
		// the row exists. Anything else unknown is not ours to track.
		if !holdable || c.pending.Hold(observation.ExchangeOrderID, func(ctx context.Context) error {
			if err := c.consume(ctx, observation, false); err != nil {
				return fmt.Errorf("replay held order event: %w", err)
			}
			return nil
		}) {
			return nil
		}
		// The row may have committed, and its pending entry been resolved,
		// between the lookup and Hold: look once more.
		order, err = c.store.OrderByExchangeID(ctx, observation.ExchangeOrderID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
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
	// The order event and the terminal result both carry the lane tag and the
	// strategy of the parent intent, so load it first. A missing intent is abnormal for an
	// order we track; publish the event with no tag and skip the result.
	intent, err := c.store.Intent(ctx, updated.IntentID)
	if errors.Is(err, store.ErrNotFound) {
		intent = store.OrderIntentRecord{}
	} else if err != nil {
		return fmt.Errorf("load order intent for result: %w", err)
	}
	if err := protocol.PublishExecutionOrderEvent(c.publish, updated.ExchangeOrderID, string(updated.State), updated.IntentID, intent.UniqueTag, intent.Strategy, updated.MatchedShares, reason, c.now()); err != nil {
		return fmt.Errorf("publish account order event: %w", err)
	}
	if intent.IntentID == "" {
		return nil
	}
	if IsQuoteIntent(intent) {
		NotifyQuoteOrder(ctx, c.onQuote, intent, updated)
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
	return PublishTerminalResultWithStore(ctx, c.aggregates, c.publish, intent, order, reason, price, c.now())
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
	if err := PublishTerminalResultWithStore(context.Background(), c.aggregates, c.publish, intent, order, reason, price, c.now()); err != nil && c.onError != nil {
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
	if IsLadderIntent(intent) {
		return ErrAggregateIntent
	}
	if IsQuoteIntent(intent) {
		// A quote level never reports on its own: the executor publishes the
		// lane's one result at its first fill (execution.quote.result).
		return nil
	}
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

// ErrAggregateIntent is returned by PublishTerminalResult for a multi-level
// intent. Its children never report on their own: one result covers them all,
// and only PublishTerminalResultWithStore can tell when that result is due.
var ErrAggregateIntent = errors.New("a multi-level intent reports one aggregated result, not one per child order")

// IsLadderIntent reports whether an intent places a multi-level ladder.
func IsLadderIntent(intent store.OrderIntentRecord) bool {
	return intentStyle(intent) == protocol.ExecutionStyleMultiLevelLimit
}

// IsQuoteIntent reports whether an intent is a quote lane's.
func IsQuoteIntent(intent store.OrderIntentRecord) bool {
	return intentStyle(intent) == protocol.ExecutionStyleQuote
}

func intentStyle(intent store.OrderIntentRecord) protocol.ExecutionStyle {
	if len(intent.Policy) == 0 {
		return ""
	}
	var policy struct {
		Style protocol.ExecutionStyle `json:"style"`
	}
	if json.Unmarshal(intent.Policy, &policy) != nil {
		return ""
	}
	return policy.Style
}

// QuoteFillHook hears of every observed change to a quote level: filled is
// true once the level is known to have matched, partially or in full. The
// executor's QuoteOrderObserved is the one implementation: the first fill on
// a lane ends it, and a level that ended unfilled stops counting as resting.
type QuoteFillHook func(ctx context.Context, intent store.OrderIntentRecord, order store.SignedOrderRecord, filled bool)

// NotifyQuoteOrder passes an observed quote level to hook.
func NotifyQuoteOrder(ctx context.Context, hook QuoteFillHook, intent store.OrderIntentRecord, order store.SignedOrderRecord) {
	if hook != nil {
		hook(ctx, intent, order, decimal.Positive(order.MatchedShares))
	}
}

// PublishTerminalResultWithStore publishes the result a child order's end
// calls for: its own for an ordinary intent, and for a multi-level intent the
// aggregated one, once every child has ended. aggregates may be nil when the
// caller's store cannot read children; that fails only for a multi-level
// intent, loudly, rather than silently dropping its result.
func PublishTerminalResultWithStore(ctx context.Context, aggregates store.AggregateOrderStore, publisher protocol.ExecutionEventPublisher, intent store.OrderIntentRecord, order store.SignedOrderRecord, reason, averagePrice string, occurredAt time.Time) error {
	if !IsLadderIntent(intent) {
		return PublishTerminalResult(publisher, intent, order, reason, averagePrice, occurredAt)
	}
	if aggregates == nil {
		return fmt.Errorf("multi-level intent %s needs an aggregate order store to publish its result", intent.IntentID)
	}
	if !statemachine.IsTerminal(order.State) {
		// This child is still open, so the aggregated result cannot be due:
		// skip the reads PublishLadderResult would make to learn that.
		return nil
	}
	return PublishLadderResult(ctx, aggregates, publisher, intent.IntentID, reason, occurredAt)
}

// PublishLadderResult publishes a multi-level intent's result when it is due
// and does nothing otherwise. It takes no lock, so it is safe to call from
// inside code that holds the intent lock. Observers race freely: the phase
// compare-and-set picks the one that publishes, and a publish that fails puts
// the phase back so the next observer retries.
//
// A result is due once placement has finished (the phase is ACTIVE or
// STOPPING, never PLACING) and every child is terminal.
func PublishLadderResult(ctx context.Context, aggregates store.AggregateOrderStore, publisher protocol.ExecutionEventPublisher, intentID, reason string, occurredAt time.Time) error {
	phase, err := aggregates.LadderPhase(ctx, intentID)
	if err != nil {
		return fmt.Errorf("read ladder phase: %w", err)
	}
	if phase != store.LadderActive && phase != store.LadderStopping {
		return nil
	}
	orders, err := aggregates.IntentOrders(ctx, intentID)
	if err != nil {
		return fmt.Errorf("load ladder children: %w", err)
	}
	if len(orders) == 0 {
		return nil
	}
	children := make([]mapping.LadderChild, len(orders))
	for i, order := range orders {
		if !statemachine.IsTerminal(order.State) {
			return nil
		}
		children[i] = mapping.LadderChild{Order: order, AveragePrice: AveragePrice(ctx, aggregates, order)}
	}
	intent, err := aggregates.Intent(ctx, intentID)
	if err != nil {
		return fmt.Errorf("load ladder intent: %w", err)
	}
	result, err := mapping.AggregateOpenResult(intent, children, reason, occurredAt)
	if err != nil {
		return err
	}
	claimed, err := aggregates.MoveLadderPhase(ctx, intentID, store.LadderPublished, store.LadderActive, store.LadderStopping)
	if err != nil || !claimed {
		return err
	}
	if err := protocol.PublishExecutionOpenResult(publisher, result); err != nil {
		if _, restoreErr := aggregates.MoveLadderPhase(ctx, intentID, phase, store.LadderPublished); restoreErr != nil {
			err = errors.Join(err, restoreErr)
		}
		return fmt.Errorf("publish aggregated open result: %w", err)
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
