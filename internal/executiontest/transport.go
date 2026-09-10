package executiontest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Cyvadra/polymarket-clob-client/internal/execution/protocol"
	"github.com/Cyvadra/polymarket-clob-client/pkg/marketquotes"
	"github.com/nats-io/nats.go"
)

type WireMessage struct {
	ReceivedAt time.Time
	Subject    string
	Payload    []byte
}

type Observer struct {
	conn         *nats.Conn
	queryTimeout time.Duration
	mu           sync.RWMutex
	messages     []WireMessage
	features map[string]protocol.PositionFeature
	quotes   []marketquotes.Snapshot
	subs     []*nats.Subscription
}

func NewObserver(ctx context.Context, cfg Config) (*Observer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	featureSubject, err := protocol.PositionFeaturesSubject(cfg.ConditionID, cfg.AssetID)
	if err != nil {
		return nil, err
	}
	conn, err := nats.Connect(cfg.NATSURL, nats.Name("polymarket-executiontest"), nats.Timeout(10*time.Second))
	if err != nil {
		return nil, fmt.Errorf("connect NATS: %w", err)
	}
	o := &Observer{conn: conn, queryTimeout: cfg.QueryTimeout, features: make(map[string]protocol.PositionFeature)}
	for _, subject := range []string{
		protocol.SubjectExecutionOpenResult,
		protocol.SubjectExecutionCloseResult,
		protocol.SubjectExecutionOrderEvent,
		featureSubject,
		protocol.SubjectMarketQuotes,
	} {
		subject := subject
		sub, subErr := conn.Subscribe(subject, func(message *nats.Msg) {
			o.record(subject, message.Data)
		})
		if subErr != nil {
			_ = conn.Drain()
			return nil, fmt.Errorf("subscribe %s: %w", subject, subErr)
		}
		o.subs = append(o.subs, sub)
	}
	if err := conn.Flush(); err != nil {
		_ = conn.Drain()
		return nil, fmt.Errorf("flush NATS subscriptions: %w", err)
	}
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	return o, nil
}

func (o *Observer) record(subject string, payload []byte) {
	message := WireMessage{ReceivedAt: time.Now().UTC(), Subject: subject, Payload: append([]byte(nil), payload...)}
	o.mu.Lock()
	o.messages = append(o.messages, message)
	if subject == protocol.SubjectMarketQuotes {
		var quote marketquotes.Snapshot
		if json.Unmarshal(payload, &quote) == nil {
			o.quotes = append(o.quotes, quote)
		}
	} else if subject != protocol.SubjectExecutionOpenResult && subject != protocol.SubjectExecutionCloseResult && subject != protocol.SubjectExecutionOrderEvent {
		var feature protocol.PositionFeature
		if json.Unmarshal(payload, &feature) == nil {
			o.features[feature.UniqueTag] = feature
		}
	}
	o.mu.Unlock()
}

func (o *Observer) Publish(subject string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", subject, err)
	}
	if err := o.conn.Publish(subject, payload); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return o.conn.Flush()
}

func (o *Observer) WaitFor(ctx context.Context, subject string, match func([]byte) bool, timeout time.Duration) (WireMessage, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		o.mu.RLock()
		for _, message := range o.messages {
			if message.Subject == subject && (match == nil || match(message.Payload)) {
				o.mu.RUnlock()
				return message, nil
			}
		}
		o.mu.RUnlock()
		select {
		case <-ctx.Done():
			return WireMessage{}, fmt.Errorf("waiting for NATS subject %s: %w", subject, ctx.Err())
		case <-deadline.C:
			return WireMessage{}, fmt.Errorf("timeout waiting for NATS subject %s", subject)
		case <-ticker.C:
		}
	}
}

// WaitForResult waits for a result message on subject whose unique_tag equals
// tag and that arrived at or after `after`. The timestamp guard matters
// because a run reuses one tag for the open and every close, so matching on
// the tag alone would return a stale terminal result.
func (o *Observer) WaitForResult(ctx context.Context, subject, tag string, after time.Time, timeout time.Duration) (WireMessage, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		o.mu.RLock()
		for _, message := range o.messages {
			if message.Subject == subject && !message.ReceivedAt.Before(after) && payloadTag(message.Payload) == tag {
				o.mu.RUnlock()
				return message, nil
			}
		}
		o.mu.RUnlock()
		select {
		case <-ctx.Done():
			return WireMessage{}, fmt.Errorf("waiting for NATS subject %s tag %s: %w", subject, tag, ctx.Err())
		case <-deadline.C:
			return WireMessage{}, fmt.Errorf("timeout waiting for NATS subject %s tag %s", subject, tag)
		case <-ticker.C:
		}
	}
}

// OrderEventsBetween returns the execution.order.event messages received in
// [start, end], decoded and in arrival order.
func (o *Observer) OrderEventsBetween(start, end time.Time) []protocol.ExecutionOrderEvent {
	o.mu.RLock()
	defer o.mu.RUnlock()
	var events []protocol.ExecutionOrderEvent
	for _, message := range o.messages {
		if message.Subject != protocol.SubjectExecutionOrderEvent {
			continue
		}
		if message.ReceivedAt.Before(start) || (!end.IsZero() && message.ReceivedAt.After(end)) {
			continue
		}
		var event protocol.ExecutionOrderEvent
		if json.Unmarshal(message.Payload, &event) == nil {
			events = append(events, event)
		}
	}
	return events
}

func payloadTag(payload []byte) string {
	var envelope struct {
		UniqueTag string `json:"unique_tag"`
	}
	_ = json.Unmarshal(payload, &envelope)
	return envelope.UniqueTag
}

func (o *Observer) Query(ctx context.Context, request protocol.PositionQueryRequest) (protocol.PositionQueryResponse, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return protocol.PositionQueryResponse{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, o.queryTimeout)
	defer cancel()
	message, err := o.conn.RequestWithContext(requestCtx, protocol.SubjectStrategyExecutionPositionQuery, payload)
	if err != nil {
		return protocol.PositionQueryResponse{}, fmt.Errorf("position query: %w", err)
	}
	var response protocol.PositionQueryResponse
	if err := json.Unmarshal(message.Data, &response); err != nil {
		return response, fmt.Errorf("decode position query response: %w", err)
	}
	if response.Error != "" {
		return response, fmt.Errorf("position query response: %s", response.Error)
	}
	return response, nil
}

func (o *Observer) Messages() []WireMessage {
	o.mu.RLock()
	defer o.mu.RUnlock()
	result := make([]WireMessage, len(o.messages))
	copy(result, o.messages)
	return result
}

func (o *Observer) Close() error {
	for _, sub := range o.subs {
		_ = sub.Unsubscribe()
	}
	if o.conn != nil {
		o.conn.Close()
	}
	return nil
}
