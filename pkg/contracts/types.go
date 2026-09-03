// Package contracts defines JSON payloads exchanged over NATS by the
// execution runtime.
package contracts

import "time"

type ExecutionEventPublisher interface {
	PublishJSON(string, any) error
}

const (
	SchemaVersionV1 = "execution.v1"

	SubjectStrategyExecutionIntent = "strategy.execution.intent"
	SubjectExecutionIntentAck      = "execution.intent.ack"
	SubjectExecutionOrderEvent     = "execution.order.event"
	SubjectPositionFeaturesPrefix  = "position.features"
)

type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

type TimeInForce string

const (
	TimeInForceGTC TimeInForce = "GTC"
	TimeInForceFOK TimeInForce = "FOK"
	TimeInForceFAK TimeInForce = "FAK"
	TimeInForceGTD TimeInForce = "GTD"
)

type IntentKind string

const (
	IntentOpen  IntentKind = "OPEN"
	IntentClose IntentKind = "CLOSE"
)

type ExecutionStyle string

const (
	ExecutionStyleLimit          ExecutionStyle = "LIMIT"
	ExecutionStyleMakerPostOnly  ExecutionStyle = "MAKER_POST_ONLY"
	ExecutionStyleTakerRepricing ExecutionStyle = "TAKER_REPRICING"
)

type ExecutionPolicy struct {
	CompleteWithinMillis int64          `json:"complete_within_ms,omitempty"`
	CancelTimeoutMillis  int64          `json:"cancel_timeout_ms,omitempty"`
	MaxFeatureAgeMillis  int64          `json:"max_feature_age_ms,omitempty"`
	MaxReprices          int            `json:"max_reprices,omitempty"`
	RepriceDelayMillis   int64          `json:"reprice_delay_ms,omitempty"`
	RepriceStep          string         `json:"reprice_step,omitempty"`
	MaxPriceDrift        string         `json:"max_price_drift,omitempty"`
	Style                ExecutionStyle `json:"style,omitempty"`
}

type ExecutionIntent struct {
	SchemaVersion      string          `json:"schema_version"`
	IntentID           string          `json:"intent_id"`
	IdempotencyKey     string          `json:"idempotency_key"`
	Strategy           string          `json:"strategy"`
	Kind               IntentKind      `json:"kind"`
	MarketID           string          `json:"market_id,omitempty"`
	EventSlug          string          `json:"event_slug,omitempty"`
	ConditionID        string          `json:"condition_id"`
	TokenID            string          `json:"token_id"`
	Outcome            string          `json:"outcome"`
	Side               Side            `json:"side"`
	TargetShares       string          `json:"target_shares"`
	LimitPrice         string          `json:"limit_price"`
	TimeInForce        TimeInForce     `json:"time_in_force"`
	PostOnly           bool            `json:"post_only"`
	FeatureSeq         int64           `json:"feature_seq,omitempty"`
	FeatureCompletedAt time.Time       `json:"feature_completed_at"`
	CreatedAt          time.Time       `json:"created_at"`
	ExpiresAt          time.Time       `json:"expires_at"`
	Policy             ExecutionPolicy `json:"policy,omitempty"`
}

type IntentAckStatus string

const (
	IntentAccepted  IntentAckStatus = "ACCEPTED"
	IntentRejected  IntentAckStatus = "REJECTED"
	IntentCompleted IntentAckStatus = "COMPLETED"
	IntentPartial   IntentAckStatus = "PARTIAL"
	IntentExpired   IntentAckStatus = "EXPIRED"
	IntentFailed    IntentAckStatus = "FAILED"
)

type ExecutionIntentAck struct {
	SchemaVersion string          `json:"schema_version"`
	IntentID      string          `json:"intent_id"`
	Status        IntentAckStatus `json:"status"`
	ReasonCode    string          `json:"reason_code,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	FilledShares  string          `json:"filled_shares,omitempty"`
	AveragePrice  string          `json:"average_price,omitempty"`
	OccurredAt    time.Time       `json:"occurred_at"`
}

func PublishExecutionIntentAck(publisher ExecutionEventPublisher, ack ExecutionIntentAck) error {
	if publisher == nil {
		return nil
	}
	ack.SchemaVersion = SchemaVersionV1
	ack.OccurredAt = ack.OccurredAt.UTC()
	return publisher.PublishJSON(SubjectExecutionIntentAck, ack)
}

type AccountFill struct {
	SchemaVersion   string    `json:"schema_version"`
	FillID          string    `json:"fill_id"`
	ExchangeOrderID string    `json:"exchange_order_id,omitempty"`
	IntentID        string    `json:"intent_id,omitempty"`
	MarketID        string    `json:"market_id,omitempty"`
	ConditionID     string    `json:"condition_id"`
	TokenID         string    `json:"token_id"`
	Outcome         string    `json:"outcome"`
	Side            Side      `json:"side"`
	Shares          string    `json:"shares"`
	Price           string    `json:"price"`
	Fee             string    `json:"fee,omitempty"`
	FeeRateBps      string    `json:"fee_rate_bps,omitempty"`
	TradeStatus     string    `json:"trade_status,omitempty"`
	TraderSide      string    `json:"trader_side,omitempty"`
	ExchangeTime    time.Time `json:"exchange_time"`
	ReceivedAt      time.Time `json:"received_at"`
}

type AccountOrderEvent struct {
	SchemaVersion   string    `json:"schema_version"`
	EventID         string    `json:"event_id"`
	ExchangeOrderID string    `json:"exchange_order_id"`
	IntentID        string    `json:"intent_id,omitempty"`
	ConditionID     string    `json:"condition_id,omitempty"`
	TokenID         string    `json:"token_id,omitempty"`
	Status          string    `json:"status"`
	MatchedShares   string    `json:"matched_shares,omitempty"`
	AveragePrice    string    `json:"average_price,omitempty"`
	ExchangeTime    time.Time `json:"exchange_time"`
	ReceivedAt      time.Time `json:"received_at"`
}

type ExecutionOrderEvent struct {
	SchemaVersion   string    `json:"schema_version"`
	IntentID        string    `json:"intent_id"`
	ExchangeOrderID string    `json:"exchange_order_id,omitempty"`
	State           string    `json:"state"`
	Reason          string    `json:"reason,omitempty"`
	MatchedShares   string    `json:"matched_shares,omitempty"`
	OccurredAt      time.Time `json:"occurred_at"`
}

func PublishExecutionOrderEvent(publisher ExecutionEventPublisher, orderID string, orderState string, intentID, matchedShares, reason string, occurredAt time.Time) error {
	if publisher == nil {
		return nil
	}
	return publisher.PublishJSON(SubjectExecutionOrderEvent, ExecutionOrderEvent{
		SchemaVersion:   SchemaVersionV1,
		IntentID:        intentID,
		ExchangeOrderID: orderID,
		State:           orderState,
		Reason:          reason,
		MatchedShares:   matchedShares,
		OccurredAt:      occurredAt.UTC(),
	})
}

type PositionFeature struct {
	SchemaVersion     string     `json:"schema_version"`
	Seq               int64      `json:"seq"`
	MarketID          string     `json:"market_id,omitempty"`
	ConditionID       string     `json:"condition_id"`
	TokenID           string     `json:"token_id"`
	Outcome           string     `json:"outcome"`
	HasPosition       bool       `json:"has_position"`
	EntryPrice        *string    `json:"entry_price"`
	EntryTime         *time.Time `json:"entry_time"`
	SecondsSinceEntry float64    `json:"seconds_since_entry"`
	PositionSize      string     `json:"position_size,omitempty"`
	AvailableSize     string     `json:"available_size,omitempty"`
	ReservedSize      string     `json:"reserved_size,omitempty"`
	State             string     `json:"state,omitempty"`
	SourceRevision    int64      `json:"source_revision,omitempty"`
	UpdatedAt         time.Time  `json:"updated_at"`
	PublishedAt       time.Time  `json:"published_at"`
}
