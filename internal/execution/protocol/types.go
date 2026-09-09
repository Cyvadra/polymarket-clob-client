// Package protocol defines versioned JSON messages exchanged by executiond
// over NATS. The external contract is documented in docs/protocol/nats-v1.md.
package protocol

import (
	"fmt"
	"strconv"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

type ExecutionEventPublisher interface {
	PublishJSON(string, any) error
}

const (
	SchemaVersionV1 = "execution.v1"

	SubjectStrategyExecutionOpen          = "strategy.execution.open"
	SubjectExecutionOpenResult            = "execution.open.result"
	SubjectStrategyExecutionClose         = "strategy.execution.close"
	SubjectExecutionCloseResult           = "execution.close.result"
	SubjectExecutionOrderEvent            = "execution.order.event"
	SubjectMarketQuotes                   = "pmm.market.quotes"
	SubjectPositionFeaturesPrefix         = "position.features"
	SubjectStrategyExecutionPositionQuery = "strategy.execution.position.query"
)

type Side = clobclient.Side

const (
	SideBuy  = clobclient.SideBuy
	SideSell = clobclient.SideSell
)

type TimeInForce = clobclient.OrderType

const (
	TimeInForceGTC = clobclient.OrderTypeGTC
	TimeInForceFOK = clobclient.OrderTypeFOK
	TimeInForceFAK = clobclient.OrderTypeFAK
	TimeInForceGTD = clobclient.OrderTypeGTD
)

type IntentKind string

const (
	IntentOpen  IntentKind = "OPEN"
	IntentClose IntentKind = "CLOSE"
)

type ExecutionStyle string

const (
	ExecutionStyleLimit           ExecutionStyle = "LIMIT"
	ExecutionStyleMakerPostOnly   ExecutionStyle = "MAKER_POST_ONLY"
	ExecutionStyleTakerAggressive ExecutionStyle = "TAKER_AGGRESSIVE"
)

type ExecutionPolicy struct {
	CompleteWithinMillis       int64          `json:"complete_within_ms,omitempty"`
	CancelTimeoutMillis        int64          `json:"cancel_timeout_ms,omitempty"`
	MaxFeatureAgeMillis        int64          `json:"max_feature_age_ms,omitempty"`
	Style                      ExecutionStyle `json:"style,omitempty"`
	MidPrice                   string         `json:"mid_price,omitempty"`
	InitialPrice               string         `json:"initial_price,omitempty"`
	MaxPrice                   string         `json:"max_price,omitempty"`
	MinPrice                   string         `json:"min_price,omitempty"`
	PriceStep                  string         `json:"price_step,omitempty"`
	QuoteOffset                string         `json:"quote_offset,omitempty"`
	RepriceIntervalMillis      int64          `json:"reprice_interval_ms,omitempty"`
	MaxReprices                int            `json:"max_reprices,omitempty"`
	QuoteMaxAgeMillis          int64          `json:"quote_max_age_ms,omitempty"`
	PostOnlyCrossRetry         bool           `json:"post_only_cross_retry,omitempty"`
	SoftCloseAfterMillis       int64          `json:"soft_close_after_ms,omitempty"`
	ForceCloseAfterMillis      int64          `json:"force_close_after_ms,omitempty"`
	CancelReplaceTimeoutMillis int64          `json:"cancel_replace_timeout_ms,omitempty"`
}

type ExecutionIntent struct {
	SchemaVersion      string          `json:"schema_version"`
	IntentID           string          `json:"intent_id"`
	UniqueTag          string          `json:"unique_tag"`
	Strategy           string          `json:"strategy"`
	Kind               IntentKind      `json:"kind"`
	MarketID           string          `json:"market_id,omitempty"`
	EventSlug          string          `json:"event_slug,omitempty"`
	ConditionID        string          `json:"condition_id"`
	TokenID            string          `json:"token_id"`
	Outcome            string          `json:"outcome"`
	Side               Side            `json:"side"`
	TargetUSD          string          `json:"target_usd,omitempty"`
	LimitPrice         string          `json:"limit_price"`
	TimeInForce        TimeInForce     `json:"time_in_force"`
	PostOnly           bool            `json:"post_only"`
	FeatureSeq         int64           `json:"feature_seq,omitempty"`
	FeatureCompletedAt time.Time       `json:"feature_completed_at"`
	CreatedAt          time.Time       `json:"created_at"`
	ExpiresAt          time.Time       `json:"expires_at"`
	Policy             ExecutionPolicy `json:"policy,omitempty"`
}

type ExecutionOpenRequest struct {
	SchemaVersion      string          `json:"schema_version"`
	UniqueTag          string          `json:"unique_tag"`
	Strategy           string          `json:"strategy"`
	MarketID           string          `json:"market_id,omitempty"`
	EventSlug          string          `json:"event_slug,omitempty"`
	ConditionID        string          `json:"condition_id"`
	TokenID            string          `json:"token_id"`
	Outcome            string          `json:"outcome"`
	Side               Side            `json:"side"`
	TargetUSD          string          `json:"target_usd,omitempty"`
	LimitPrice         string          `json:"limit_price"`
	TimeInForce        TimeInForce     `json:"time_in_force"`
	PostOnly           bool            `json:"post_only"`
	FeatureSeq         int64           `json:"feature_seq,omitempty"`
	FeatureCompletedAt time.Time       `json:"feature_completed_at"`
	CreatedAt          time.Time       `json:"created_at"`
	ExpiresAt          time.Time       `json:"expires_at"`
	Policy             ExecutionPolicy `json:"policy,omitempty"`
}

type ExecutionCloseMode string

const (
	ExecutionCloseModeLimit ExecutionCloseMode = "LIMIT_CLOSE"
	ExecutionCloseModeForce ExecutionCloseMode = "FORCE_CLOSE"
)

type ExecutionCloseRequest struct {
	SchemaVersion string             `json:"schema_version"`
	UniqueTag     string             `json:"unique_tag"`
	Strategy      string             `json:"strategy"`
	ConditionID   string             `json:"condition_id"`
	AssetID       string             `json:"asset_id"`
	Outcome       string             `json:"outcome"`
	Mode          ExecutionCloseMode `json:"mode"`
	LimitPrice    string             `json:"limit_price,omitempty"`
	TimeInForce   TimeInForce        `json:"time_in_force,omitempty"`
	CreatedAt     time.Time          `json:"created_at"`
	Policy        ExecutionPolicy    `json:"policy,omitempty"`
}

// Reason codes carried by result messages. They are part of the wire contract,
// so they are declared once here rather than derived from error text.
const (
	ReasonInvalidIntent         = "INVALID_INTENT"
	ReasonUnsupportedStyle      = "UNSUPPORTED_EXECUTION_STYLE"
	ReasonUnimplementedPolicy   = "UNIMPLEMENTED_POLICY"
	ReasonNoPosition            = "NO_POSITION"
	ReasonActiveSellReservation = "ACTIVE_SELL_RESERVATION"
	ReasonExposureLimit         = "EXPOSURE_LIMIT"
	ReasonUnplannable           = "UNPLANNABLE"
	ReasonOrderRejected         = "ORDER_REJECTED"
	ReasonExecutionFailed       = "EXECUTION_FAILED"
)

type ResultStatus string

const (
	ResultSucceeded ResultStatus = "SUCCEEDED"
	ResultFailed    ResultStatus = "FAILED"
)

type ExecutionOpenResult struct {
	SchemaVersion string       `json:"schema_version"`
	UniqueTag     string       `json:"unique_tag"`
	ConditionID   string       `json:"condition_id"`
	TokenID       string       `json:"token_id"`
	Outcome       string       `json:"outcome"`
	Side          Side         `json:"side"`
	Status        ResultStatus `json:"status"`
	ReasonCode    string       `json:"reason_code,omitempty"`
	Reason        string       `json:"reason,omitempty"`
	FilledShares  float64      `json:"filled_shares,omitempty"`
	AveragePrice  float64      `json:"average_price,omitempty"`
	OccurredAt    time.Time    `json:"occurred_at"`
}

type ExecutionCloseResult struct {
	SchemaVersion string       `json:"schema_version"`
	UniqueTag     string       `json:"unique_tag"`
	ConditionID   string       `json:"condition_id"`
	AssetID       string       `json:"asset_id"`
	Outcome       string       `json:"outcome"`
	Side          Side         `json:"side"`
	Status        ResultStatus `json:"status"`
	ReasonCode    string       `json:"reason_code,omitempty"`
	Reason        string       `json:"reason,omitempty"`
	FilledShares  float64      `json:"filled_shares,omitempty"`
	AveragePrice  float64      `json:"average_price,omitempty"`
	OccurredAt    time.Time    `json:"occurred_at"`
}

func PublishExecutionOpenResult(publisher ExecutionEventPublisher, result ExecutionOpenResult) error {
	if publisher == nil {
		return nil
	}
	result.SchemaVersion = SchemaVersionV1
	result.OccurredAt = result.OccurredAt.UTC()
	return publisher.PublishJSON(SubjectExecutionOpenResult, result)
}

func PublishExecutionCloseResult(publisher ExecutionEventPublisher, result ExecutionCloseResult) error {
	if publisher == nil {
		return nil
	}
	result.SchemaVersion = SchemaVersionV1
	result.OccurredAt = result.OccurredAt.UTC()
	return publisher.PublishJSON(SubjectExecutionCloseResult, result)
}

type ExecutionOrderEvent struct {
	SchemaVersion   string    `json:"schema_version"`
	IntentID        string    `json:"intent_id"`
	ExchangeOrderID string    `json:"exchange_order_id,omitempty"`
	State           string    `json:"state"`
	Reason          string    `json:"reason,omitempty"`
	MatchedShares   float64   `json:"matched_shares,omitempty"`
	OccurredAt      time.Time `json:"occurred_at"`
}

func PublishExecutionOrderEvent(publisher ExecutionEventPublisher, orderID string, orderState string, intentID, matchedShares, reason string, occurredAt time.Time) error {
	if publisher == nil {
		return nil
	}
	parsedShares, err := strconv.ParseFloat(matchedShares, 64)
	if err != nil {
		return fmt.Errorf("invalid matched_shares %q: %w", matchedShares, err)
	}
	return publisher.PublishJSON(SubjectExecutionOrderEvent, ExecutionOrderEvent{
		SchemaVersion:   SchemaVersionV1,
		IntentID:        intentID,
		ExchangeOrderID: orderID,
		State:           orderState,
		Reason:          reason,
		MatchedShares:   parsedShares,
		OccurredAt:      occurredAt.UTC(),
	})
}

type PositionQueryRequest struct {
	SchemaVersion string `json:"schema_version"`
	ConditionID   string `json:"condition_id,omitempty"`
	MarketID      string `json:"market_id,omitempty"`
	UniqueTag     string `json:"unique_tag,omitempty"`
}

type PositionQueryResponse struct {
	SchemaVersion string            `json:"schema_version"`
	Positions     []PositionFeature `json:"positions"`
	Error         string            `json:"error,omitempty"`
}

type PositionFeature struct {
	SchemaVersion     string     `json:"schema_version"`
	Seq               int64      `json:"seq"`
	UniqueTag         string     `json:"unique_tag,omitempty"`
	MarketID          string     `json:"market_id,omitempty"`
	ConditionID       string     `json:"condition_id"`
	TokenID           string     `json:"token_id"`
	Outcome           string     `json:"outcome"`
	HasPosition       bool       `json:"has_position"`
	EntryPrice        *float64   `json:"entry_price"`
	EntryTime         *time.Time `json:"entry_time"`
	SecondsSinceEntry float64    `json:"seconds_since_entry"`
	PositionSize      float64    `json:"position_size,omitempty"`
	ActualShares      float64    `json:"actual_shares,omitempty"`
	AvailableSize     float64    `json:"available_size,omitempty"`
	ReservedSize      float64    `json:"reserved_size,omitempty"`
	State             string     `json:"state,omitempty"`
	SourceRevision    int64      `json:"source_revision,omitempty"`
	UpdatedAt         time.Time  `json:"updated_at"`
	PublishedAt       time.Time  `json:"published_at"`
}
